package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
)

const markerUser = "33333333-3333-4333-8333-333333333333"

type sharingClient struct {
	immich.Client
	shares, lists int
	failShare     bool
}

func (c *sharingClient) AddAlbumUser(ctx context.Context, m domain.Member, albumID, userID string) error {
	c.shares++
	if c.failShare {
		return errors.New("sharing unavailable")
	}
	return c.Client.AddAlbumUser(ctx, m, albumID, userID)
}

func (c *sharingClient) ListAlbums(ctx context.Context, m domain.Member) ([]domain.Album, error) {
	c.lists++
	return c.Client.ListAlbums(ctx, m)
}

func TestTogetherSharingDiscoversOwnedAlbumsAndBackfillsLegacy(t *testing.T) {
	_, db, api, r := setup(t)
	ctx := context.Background()
	if _, err := r.EnsureTogether(ctx); err != nil {
		t.Fatal(err)
	}
	// A pre-feature registration has no sharing marker yet.
	if _, err := r.Register(ctx, "alice", "best"); err != nil {
		t.Fatal(err)
	}
	if err := api.AddUser(markerUser); err != nil {
		t.Fatal(err)
	}
	if err := api.AddUser("unrelated-user"); err != nil {
		t.Fatal(err)
	}
	r.C.TogetherUserID = markerUser
	m := r.C.Members
	if err := api.AddAlbumUser(ctx, m[0], "trip", markerUser); err != nil {
		t.Fatal(err)
	}
	bobAlbum, err := api.CreateAlbum(ctx, m[1], "bob-source", "Trip", "Bob's trip")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.AddAssets(ctx, m[1], bobAlbum.ID, []string{"b1"}); err != nil {
		t.Fatal(err)
	}
	if err := api.AddAlbumUser(ctx, m[1], bobAlbum.ID, markerUser); err != nil {
		t.Fatal(err)
	}
	unmarked, err := api.CreateAlbum(ctx, m[2], "unmarked-source", "Trip", "Not mirrored")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.AddAlbumUser(ctx, m[2], unmarked.ID, "unrelated-user"); err != nil {
		t.Fatal(err)
	}
	measured := &sharingClient{Client: api}
	r.API = measured
	for i := 0; i < 2; i++ {
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	albums, err := db.Albums()
	if err != nil || len(albums) != 4 {
		t.Fatalf("want Together, legacy Best, and two distinct Trips: %+v, %v", albums, err)
	}
	if _, found, err := db.AlbumByReplica(m[2].ID, unmarked.ID); err != nil || found {
		t.Fatalf("ordinary shared album registered: %v, %v", found, err)
	}
	for _, album := range albums {
		reps, err := db.AlbumReplicas(album.ID)
		if err != nil || len(reps) != len(m) {
			t.Fatalf("replicas=%v, err=%v", reps, err)
		}
		for _, member := range m {
			remote, err := api.GetAlbum(ctx, member, reps[member.ID])
			if err != nil || !r.sharedWithTogether(remote) {
				t.Fatalf("album not marked: %+v, %v", remote, err)
			}
			for _, user := range remote.Users {
				if user.UserID == markerUser && user.Role != "viewer" {
					t.Fatalf("marker role=%q", user.Role)
				}
			}
			assets, err := api.ListAlbumAssets(ctx, member, remote.ID)
			want := 1
			if album.SystemKey == "together" {
				want = 2
			}
			if err != nil || len(assets) != want {
				t.Fatalf("%s/%s assets=%d, want=%d, err=%v", album.Name, member.ID, len(assets), want, err)
			}
		}
	}
	shares, lists := measured.shares, measured.lists
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if measured.shares != shares {
		t.Fatal("idempotent discovery repeated share writes")
	}
	if err := r.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if measured.lists != lists+len(m) || measured.shares != shares {
		t.Fatal("work pass performed marker discovery or sharing")
	}
}

func TestTogetherShareAPIRegistrationFailureRetryAndRepair(t *testing.T) {
	_, db, api, r := setup(t)
	ctx := context.Background()
	r.C.TogetherUserID = markerUser
	if err := api.AddUser(markerUser); err != nil {
		t.Fatal(err)
	}
	measured := &sharingClient{Client: api, failShare: true}
	r.API = measured
	id, err := r.Register(ctx, "alice", "trip")
	if id == "" || err == nil || !strings.Contains(err.Error(), "sharing unavailable") {
		t.Fatalf("registration did not report failed sharing: %q, %v", id, err)
	}
	measured.failShare = false
	retryID, err := r.Register(ctx, "alice", "trip")
	if err != nil || retryID != id {
		t.Fatalf("retry duplicated registration: %q, %v", retryID, err)
	}
	reps, err := db.AlbumReplicas(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range r.C.Members {
		remote, err := api.GetAlbum(ctx, member, reps[member.ID])
		if err != nil || !r.sharedWithTogether(remote) {
			t.Fatalf("API registration did not mark replica: %+v, %v", remote, err)
		}
	}
	if err := api.SetAlbumUsers("trip", []domain.AlbumUser{{UserID: "other", Role: "editor"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	remote, err := api.GetAlbum(ctx, r.C.Members[0], "trip")
	if err != nil || !r.sharedWithTogether(remote) || len(remote.Users) != 2 || remote.Users[0].Role != "editor" {
		t.Fatalf("repair changed other participants: %+v, %v", remote, err)
	}
	albums, _ := db.Albums()
	if len(albums) != 1 {
		t.Fatal("repair created another logical album")
	}
}

func TestTogetherShareDryRunRegistersLocallyWithoutImmichWrites(t *testing.T) {
	_, db, api, r := setup(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "alice", "best"); err != nil {
		t.Fatal(err)
	}
	if err := api.AddUser(markerUser); err != nil {
		t.Fatal(err)
	}
	if err := api.AddAlbumUser(ctx, r.C.Members[0], "trip", markerUser); err != nil {
		t.Fatal(err)
	}
	r.C.TogetherUserID, r.C.DryRun = markerUser, true
	measured := &sharingClient{Client: api}
	r.API = measured
	before := 0
	for _, member := range r.C.Members {
		albums, _ := api.ListAlbums(ctx, member)
		before += len(albums)
	}
	actions, err := r.DryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var registered, share bool
	for _, action := range actions {
		registered = registered || action.Kind == "register_mirror_album"
		share = share || action.Kind == "share_album"
	}
	if !registered || !share || measured.shares != 0 {
		t.Fatalf("dry-run actions=%+v, writes=%d", actions, measured.shares)
	}
	if _, found, err := db.AlbumByReplica("alice", "trip"); err != nil || !found {
		t.Fatalf("dry-run did not persist local registration: %v, %v", found, err)
	}
	after := 0
	for _, member := range r.C.Members {
		albums, _ := api.ListAlbums(ctx, member)
		after += len(albums)
	}
	if before != after {
		t.Fatal("dry-run created Immich albums")
	}
	r.C.DryRun = false
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if measured.shares == 0 {
		t.Fatal("activation did not apply pending shares")
	}
}

func TestTogetherShareIsOptInAndChecksPermissionsAndUser(t *testing.T) {
	_, db, api, r := setup(t)
	ctx := context.Background()
	r.API = restrictedClient{Client: api}
	if err := r.Check(ctx); err != nil {
		t.Fatal(err)
	}
	r.C.TogetherUserID = markerUser
	if err := r.Check(ctx); err == nil || !strings.Contains(err.Error(), "albumUser.create") {
		t.Fatalf("accepted missing share permission: %v", err)
	}
	r.API = api
	if err := r.Check(ctx); err == nil || !strings.Contains(err.Error(), "existing Immich user") {
		t.Fatalf("accepted missing marker user: %v", err)
	}
	if err := api.AddUser(markerUser); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := api.AddAlbumUser(ctx, r.C.Members[0], "trip", markerUser); err != nil {
		t.Fatal(err)
	}
	r.C.TogetherUserID = ""
	measured := &sharingClient{Client: api}
	r.API = measured
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	albums, _ := db.Albums()
	if len(albums) != 0 || measured.lists != 0 || measured.shares != 0 {
		t.Fatal("disabled feature discovered/shared albums")
	}
}

func TestActivePreviewDiscoveryDoesNotWriteAndExistingMarkerRoleIsPreserved(t *testing.T) {
	_, db, api, r := setup(t)
	ctx := context.Background()
	r.C.TogetherUserID = markerUser
	if err := api.AddUser(markerUser); err != nil {
		t.Fatal(err)
	}
	if err := api.SetAlbumUsers("trip", []domain.AlbumUser{{UserID: markerUser, Role: "editor"}}); err != nil {
		t.Fatal(err)
	}
	measured := &sharingClient{Client: api}
	r.API = measured
	if _, err := r.DryRun(ctx); err != nil {
		t.Fatal(err)
	}
	if measured.shares != 0 {
		t.Fatal("preview on an active reconciler wrote sharing changes")
	}
	id, found, err := db.AlbumByReplica("alice", "trip")
	if err != nil || !found {
		t.Fatalf("missing local preview registration: %v, %v", found, err)
	}
	reps, _ := db.AlbumReplicas(id)
	if len(reps) != 1 {
		t.Fatal("preview on an active reconciler created remote replicas")
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	remote, err := api.GetAlbum(ctx, r.C.Members[0], "trip")
	if err != nil || len(remote.Users) != 1 || remote.Users[0].Role != "editor" {
		t.Fatalf("existing marker role overwritten: %+v, %v", remote, err)
	}
}
