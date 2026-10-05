package reconcile

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
	"github.com/0x464e/immich-family-bridge/internal/store"
)

type albumFailureClient struct {
	immich.Client
	failDeleteMember, failReadMember, failSnapshotMember, failAsset, missingAsset string
	loseDeleteResponse, loseCreateResponse                                        bool
	deletes, creates                                                              int
	dropAdds                                                                      bool
}

func (c *albumFailureClient) AddAssets(ctx context.Context, m domain.Member, id string, assets []string) error {
	if c.dropAdds {
		return nil
	}
	return c.Client.AddAssets(ctx, m, id, assets)
}

func (c *albumFailureClient) DeleteAlbum(ctx context.Context, m domain.Member, id string) error {
	c.deletes++
	if m.ID == c.failDeleteMember {
		return errors.New("delete unavailable")
	}
	err := c.Client.DeleteAlbum(ctx, m, id)
	if c.loseDeleteResponse && err == nil {
		c.loseDeleteResponse = false
		return errors.New("lost delete response")
	}
	return err
}

func (c *albumFailureClient) CreateAlbum(ctx context.Context, m domain.Member, logical, name, description string) (domain.Album, error) {
	c.creates++
	a, err := c.Client.CreateAlbum(ctx, m, logical, name, description)
	if c.loseCreateResponse && err == nil {
		c.loseCreateResponse = false
		return domain.Album{}, errors.New("lost create response")
	}
	return a, err
}

func (c *albumFailureClient) GetAlbum(ctx context.Context, m domain.Member, id string) (domain.Album, error) {
	if m.ID == c.failReadMember {
		return domain.Album{}, errors.New("read unavailable")
	}
	return c.Client.GetAlbum(ctx, m, id)
}

func (c *albumFailureClient) ListAlbumAssets(ctx context.Context, m domain.Member, id string) ([]domain.Asset, error) {
	if m.ID == c.failSnapshotMember {
		return nil, errors.New("snapshot unavailable")
	}
	return c.Client.ListAlbumAssets(ctx, m, id)
}

func (c *albumFailureClient) GetAsset(ctx context.Context, m domain.Member, id string) (domain.Asset, error) {
	if id == c.failAsset {
		return domain.Asset{}, errors.New("asset read unavailable")
	}
	if id == c.missingAsset {
		return domain.Asset{}, immich.ErrNotFound
	}
	return c.Client.GetAsset(ctx, m, id)
}

func deletionFixture(t *testing.T) (*store.Store, *Reconciler, string, string) {
	t.Helper()
	_, db, api, r := setup(t)
	r.C.TogetherUserID = markerUser
	ctx := context.Background()
	if err := api.AddUser(markerUser); err != nil {
		t.Fatal(err)
	}
	together, err := r.EnsureTogether(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.Register(ctx, "alice", "trip")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return db, r, id, together
}

func TestUnmirrorDeletesAllAlbumsButRetainsPhotosAndTogetherThenRestores(t *testing.T) {
	db, r, id, together := deletionFixture(t)
	r.C.RemoveUnshared = true // Album deletion must retain media even with asset cleanup enabled.
	ctx := context.Background()
	old, _ := db.AlbumReplicas(id)
	togetherReplicas, _ := db.AlbumReplicas(together)
	assets, _ := db.Replicas()
	if err := r.UnmirrorAlbum(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := r.UnmirrorAlbum(ctx, id); err != nil {
		t.Fatal("repeat delete:", err)
	}
	record, found, err := db.AlbumDeletion(id)
	if err != nil || !found || record.State != "deleted" || record.Album.Name != "Trip" {
		t.Fatalf("archive=%+v, %v", record, err)
	}
	for _, member := range r.C.Members {
		if _, err := r.API.GetAlbum(ctx, member, old[member.ID]); !errors.Is(err, immich.ErrNotFound) {
			t.Fatalf("album survived: %v", err)
		}
		items, err := r.API.ListAlbumAssets(ctx, member, togetherReplicas[member.ID])
		if err != nil || len(items) != 1 {
			t.Fatalf("Together changed: %v %v", items, err)
		}
	}
	for _, asset := range assets {
		m, _ := r.member(asset.MemberID)
		if _, err := r.API.GetAsset(ctx, m, asset.AssetID); err != nil {
			t.Fatal("asset deleted:", err)
		}
		if _, err := os.Stat(asset.Path); err != nil {
			t.Fatal("file deleted:", err)
		}
	}
	if _, err := r.Register(ctx, "alice", old["alice"]); err == nil {
		t.Fatal("archived source re-registered")
	}
	if _, found, err := db.ResolveMirrorAlbum(ctx, old["alice"], r.C.Members[1].UserID); err != nil || found {
		t.Fatal("archived album link resolves")
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal("post-delete cycle:", err)
	}
	if err := r.RestoreAlbum(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.AlbumDeletion(id); err != nil || found {
		t.Fatal("successful restore retained deletion record")
	}
	newIDs, _ := db.AlbumReplicas(id)
	if err := r.RestoreAlbum(ctx, id); err != nil {
		t.Fatal("repeat restore:", err)
	}
	for _, member := range r.C.Members {
		if newIDs[member.ID] == old[member.ID] {
			t.Fatal("restore reused old remote ID")
		}
		remote, err := r.API.GetAlbum(ctx, member, newIDs[member.ID])
		if err != nil || remote.Name != "Trip" || strings.Contains(remote.Description, "familybridge restore") || !r.sharedWithTogether(remote) {
			t.Fatalf("restore=%+v %v", remote, err)
		}
		items, _ := r.API.ListAlbumAssets(ctx, member, remote.ID)
		if len(items) != 1 {
			t.Fatalf("restored contents=%v", items)
		}
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal("post-restore cycle:", err)
	}
	if again, _ := db.AlbumReplicas(id); again["alice"] != newIDs["alice"] {
		t.Fatal("restore rediscovered as new album")
	}
}

func TestAnyMemberUnshareDeletesGloballyAndTogetherIsProtected(t *testing.T) {
	db, r, id, together := deletionFixture(t)
	ctx := context.Background()
	api := r.API.(interface {
		SetAlbumUsers(string, []domain.AlbumUser) error
	})
	replicas, _ := db.AlbumReplicas(id)
	togetherReplicas, _ := db.AlbumReplicas(together)
	if err := api.SetAlbumUsers(replicas["bob"], nil); err != nil {
		t.Fatal(err)
	}
	// Re-registering cannot silently restore a user's removed share.
	if _, err := r.Register(ctx, "bob", replicas["bob"]); err == nil {
		t.Fatal("registration undid unmirror intent")
	}
	if err := api.SetAlbumUsers(togetherReplicas["carol"], nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	record, _, _ := db.AlbumDeletion(id)
	if record.State != "deleted" || record.RequestedBy != "member:bob" {
		t.Fatalf("wrong UI deletion: %+v", record)
	}
	if err := r.UnmirrorAlbum(ctx, together); !errors.Is(err, ErrProtectedAlbum) {
		t.Fatal("Together API deletion accepted:", err)
	}
	if err := r.RestoreAlbum(ctx, together); !errors.Is(err, ErrProtectedAlbum) {
		t.Fatal("Together API restore accepted:", err)
	}
	remote, err := r.API.GetAlbum(ctx, r.C.Members[2], togetherReplicas["carol"])
	if err != nil || !r.sharedWithTogether(remote) {
		t.Fatal("Together UI unshare did not repair")
	}
}

func TestUnmirrorReadsMustSucceedBeforeAnyDeletionAndDryRunDoesNotWrite(t *testing.T) {
	db, r, id, _ := deletionFixture(t)
	ctx := context.Background()
	client := &albumFailureClient{Client: r.API, failSnapshotMember: "carol"}
	r.API = client
	if err := r.UnmirrorAlbum(ctx, id); err == nil {
		t.Fatal("snapshot failure ignored")
	}
	if _, archived, _ := db.AlbumDeletion(id); archived || client.deletes != 0 {
		t.Fatal("incomplete snapshot mutated albums")
	}
	client.failSnapshotMember, client.failReadMember = "", "bob"
	if err := r.Run(ctx); err == nil || client.deletes != 0 {
		t.Fatal("read failure triggered deletion")
	}
	client.failReadMember = ""
	reps, _ := db.AlbumReplicas(id)
	api := client.Client.(interface {
		SetAlbumUsers(string, []domain.AlbumUser) error
	})
	if err := api.SetAlbumUsers(reps["carol"], nil); err != nil {
		t.Fatal(err)
	}
	r.C.DryRun = true
	if err := r.UnmirrorAlbum(ctx, id); !errors.Is(err, ErrDryRunMode) {
		t.Fatal(err)
	}
	if err := r.RestoreAlbum(ctx, id); !errors.Is(err, ErrDryRunMode) {
		t.Fatal(err)
	}
	actions, err := r.DryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	preview := false
	for _, a := range actions {
		preview = preview || a.Kind == "unmirror_album" && a.AlbumID == id
	}
	if !preview || client.deletes != 0 {
		t.Fatal("dry-run did not safely preview unmirror")
	}
	if _, archived, _ := db.AlbumDeletion(id); archived {
		t.Fatal("dry-run persisted deletion intent")
	}
	r.C.DryRun = false
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAlbumDeletionAndRestoreResumeAfterLostResponsesAndRestart(t *testing.T) {
	db, r, id, _ := deletionFixture(t)
	ctx := context.Background()
	client := &albumFailureClient{Client: r.API, loseDeleteResponse: true}
	r.API = client
	if err := r.UnmirrorAlbum(ctx, id); err == nil {
		t.Fatal("lost response not reported")
	}
	record, archived, _ := db.AlbumDeletion(id)
	if !archived || record.State != "deleting" {
		t.Fatal("intent not durable")
	}
	if albums, _ := db.Albums(); len(albums) != 1 {
		t.Fatal("pending album still active")
	}
	if err := r.RestoreAlbum(ctx, id); err == nil {
		t.Fatal("restored while deletion incomplete")
	}
	r = New(r.C, db, client, r.Log)
	if err := r.Work(ctx); err != nil {
		t.Fatal("delete resume:", err)
	}
	record, _, _ = db.AlbumDeletion(id)
	if record.State != "deleted" {
		t.Fatal("delete resume incomplete")
	}
	client.loseCreateResponse = true
	if err := r.RestoreAlbum(ctx, id); err == nil {
		t.Fatal("lost create response not reported")
	}
	r = New(r.C, db, client, r.Log)
	if err := r.Work(ctx); err != nil {
		t.Fatal("restore resume:", err)
	}
	if client.creates != len(r.C.Members) {
		t.Fatalf("lost response duplicated creation: %d", client.creates)
	}
	if _, found, _ := db.AlbumDeletion(id); found {
		t.Fatal("recovery record not cleared")
	}
}

func TestRestoreSkipsMissingAssetsButRetriesOtherReadFailures(t *testing.T) {
	db, r, id, _ := deletionFixture(t)
	ctx := context.Background()
	if err := r.UnmirrorAlbum(ctx, id); err != nil {
		t.Fatal(err)
	}
	record, _, _ := db.AlbumDeletion(id)
	record.Replicas[0].AssetIDs = append(record.Replicas[0].AssetIDs, "missing-id")
	record.Replicas[0].CoverID = "missing-id"
	if err := db.SaveAlbumDeletion(record); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	r.Log = slog.New(slog.NewTextHandler(&logs, nil))
	client := &albumFailureClient{Client: r.API, missingAsset: "missing-id", failAsset: "a1"}
	r.API = client
	if err := r.RestoreAlbum(ctx, id); err == nil {
		t.Fatal("transient read treated as missing")
	}
	if record, _, _ := db.AlbumDeletion(id); record.State != "restoring" {
		t.Fatal("failed restore lost archive")
	}
	client.failAsset = ""
	if err := r.RestoreAlbum(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "skipped missing asset") || !strings.Contains(logs.String(), "missing-id") {
		t.Fatal("missing asset not logged")
	}
	if client.creates != len(r.C.Members) {
		t.Fatal("retry duplicated albums")
	}
	reps, _ := db.AlbumReplicas(id)
	items, err := r.API.ListAlbumAssets(ctx, r.C.Members[0], reps["alice"])
	if err != nil || len(items) != 1 || items[0].ID != "a1" {
		t.Fatalf("restored assets=%v %v", items, err)
	}
}

func TestRecoveryRecordCannotDeleteTogetherReplica(t *testing.T) {
	db, r, id, together := deletionFixture(t)
	ctx := context.Background()
	client := &albumFailureClient{Client: r.API, failDeleteMember: "alice"}
	r.API = client
	if err := r.UnmirrorAlbum(ctx, id); err == nil {
		t.Fatal("expected failure")
	}
	record, _, _ := db.AlbumDeletion(id)
	togetherReplicas, _ := db.AlbumReplicas(together)
	record.Replicas[0].AlbumID = togetherReplicas["alice"]
	if err := db.SaveAlbumDeletion(record); err != nil {
		t.Fatal(err)
	}
	client.failDeleteMember = ""
	if err := r.UnmirrorAlbum(ctx, id); !errors.Is(err, ErrProtectedAlbum) {
		t.Fatal("corrupt archive targeted Together:", err)
	}
	if _, err := r.API.GetAlbum(ctx, r.C.Members[0], togetherReplicas["alice"]); err != nil {
		t.Fatal("Together deleted")
	}
}

func TestRestoreVerifiesMembershipBeforeRemovingRecord(t *testing.T) {
	db, r, id, _ := deletionFixture(t)
	ctx := context.Background()
	if err := r.UnmirrorAlbum(ctx, id); err != nil {
		t.Fatal(err)
	}
	client := &albumFailureClient{Client: r.API, dropAdds: true}
	r.API = client
	if err := r.RestoreAlbum(ctx, id); err == nil || !strings.Contains(err.Error(), "missing asset") {
		t.Fatal("silently failed membership lost recovery record:", err)
	}
	if record, found, _ := db.AlbumDeletion(id); !found || record.State != "restoring" {
		t.Fatal("missing recovery record")
	}
	client.dropAdds = false
	if err := r.RestoreAlbum(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := db.AlbumDeletion(id); found {
		t.Fatal("completed restore retained record")
	}
}

func TestDeletionSnapshotsUnobservedMemberEditsAndEmptyAlbums(t *testing.T) {
	db, r, id, _ := deletionFixture(t)
	r.scanInterval = 0 // Import a second origin immediately in this focused fake test.
	ctx := context.Background()
	reps, _ := db.AlbumReplicas(id)
	if err := r.API.AddAssets(ctx, r.C.Members[1], reps["bob"], []string{"b1"}); err != nil {
		t.Fatal(err)
	}
	if err := r.UnmirrorAlbum(ctx, id); err != nil {
		t.Fatal(err)
	}
	record, _, _ := db.AlbumDeletion(id)
	if len(record.Replicas[1].AssetIDs) != 2 {
		t.Fatal("snapshot lost edit not yet reconciled")
	}
	if err := r.RestoreAlbum(ctx, id); err != nil {
		t.Fatal(err)
	}
	reps, _ = db.AlbumReplicas(id)
	items, err := r.API.ListAlbumAssets(ctx, r.C.Members[1], reps["bob"])
	if err != nil || len(items) != 2 {
		t.Fatal("restore lost member edit")
	}
	for i := 0; i < 2; i++ {
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, member := range r.C.Members {
		items, err := r.API.ListAlbumAssets(ctx, member, reps[member.ID])
		if err != nil || len(items) != 2 {
			t.Fatal("restored album did not resume mirroring")
		}
	}
	empty, err := r.API.CreateAlbum(ctx, r.C.Members[0], "empty-source", "Empty", "")
	if err != nil {
		t.Fatal(err)
	}
	emptyID, err := r.Register(ctx, "alice", empty.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.UnmirrorAlbum(ctx, emptyID); err != nil {
		t.Fatal(err)
	}
	if err := r.RestoreAlbum(ctx, emptyID); err != nil {
		t.Fatal(err)
	}
	reps, _ = db.AlbumReplicas(emptyID)
	for _, member := range r.C.Members {
		items, err := r.API.ListAlbumAssets(ctx, member, reps[member.ID])
		if err != nil || len(items) != 0 {
			t.Fatal("empty album lifecycle changed contents")
		}
	}
}
