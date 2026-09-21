package reconcile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/config"
	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
	fake "github.com/0x464e/immich-family-bridge/internal/immich/testfake"
	"github.com/0x464e/immich-family-bridge/internal/store"
)

type measuringClient struct {
	immich.Client
	scans             map[string]int
	metadataRefreshes map[string]int
	addSizes          []int
	albumReads        int
	albumSearches     int
	albumUpdates      int
}

type cancelingClient struct {
	immich.Client
	cancel context.CancelFunc
}

type restrictedClient struct{ immich.Client }

func (restrictedClient) Permissions(context.Context, domain.Member) ([]string, error) {
	return []string{"asset.read", "stack.read", "stack.create", "stack.delete"}, nil
}

func TestCleanupRequiresAssetDeletePermission(t *testing.T) {
	c, db, api, _ := setup(t)
	c.RemoveUnshared = true
	r := New(c, db, restrictedClient{Client: api}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := r.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "asset.delete") {
		t.Fatalf("cleanup accepted restricted member key: %v", err)
	}
}

func (c cancelingClient) GetAsset(ctx context.Context, member domain.Member, id string) (domain.Asset, error) {
	c.cancel()
	return domain.Asset{}, ctx.Err()
}

func TestCanceledCycleDoesNotWarnOrCorruptMappings(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	cycleCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var output bytes.Buffer
	interrupted := New(c, db, cancelingClient{Client: api, cancel: cancel}, slog.New(slog.NewJSONHandler(&output, nil)))
	if err := interrupted.Run(cycleCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled cycle, got %v", err)
	}
	if strings.Contains(output.String(), `"level":"WARN"`) || strings.Contains(output.String(), `"level":"ERROR"`) {
		t.Fatalf("shutdown cancellation produced warnings: %s", output.String())
	}
	if err := r.Run(ctx); err != nil {
		t.Fatalf("cycle after interruption: %v", err)
	}
}

func (m *measuringClient) ScanLibrary(ctx context.Context, member domain.Member) error {
	m.scans[member.ID]++
	return m.Client.ScanLibrary(ctx, member)
}

func (m *measuringClient) AddAssets(ctx context.Context, member domain.Member, albumID string, ids []string) error {
	m.addSizes = append(m.addSizes, len(ids))
	return m.Client.AddAssets(ctx, member, albumID, ids)
}

func (m *measuringClient) RefreshMetadata(ctx context.Context, member domain.Member, ids []string) error {
	m.metadataRefreshes[member.ID] += len(ids)
	return m.Client.RefreshMetadata(ctx, member, ids)
}

func (m *measuringClient) GetAlbum(ctx context.Context, member domain.Member, id string) (domain.Album, error) {
	m.albumReads++
	return m.Client.GetAlbum(ctx, member, id)
}

func (m *measuringClient) ListAlbumAssets(ctx context.Context, member domain.Member, id string) ([]domain.Asset, error) {
	m.albumSearches++
	return m.Client.ListAlbumAssets(ctx, member, id)
}

func (m *measuringClient) UpdateAlbum(ctx context.Context, member domain.Member, id, name, desc, cover string) error {
	m.albumUpdates++
	return m.Client.UpdateAlbum(ctx, member, id, name, desc, cover)
}

func TestWorkUsesPersistedDecisionsWithoutAlbumSearch(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	if _, err := r.EnsureTogether(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Work(ctx); err != nil {
		t.Fatal(err)
	}
	measured := &measuringClient{Client: api, scans: map[string]int{}}
	worker := New(c, db, measured, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := worker.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if measured.albumReads != 0 || measured.albumSearches != 0 || measured.albumUpdates != 0 {
		t.Fatalf("steady work touched albums: reads=%d searches=%d updates=%d", measured.albumReads, measured.albumSearches, measured.albumUpdates)
	}
	albums, err := db.Albums()
	if err != nil {
		t.Fatal(err)
	}
	var togetherID string
	for _, album := range albums {
		if album.SystemKey == "together" {
			togetherID = album.ID
		}
	}
	if togetherID == "" {
		t.Fatal("Together album missing")
	}
	reps, err := db.AlbumReplicas(togetherID)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.AddAssets(ctx, c.Members[1], reps["bob"], []string{"b1"}); err != nil {
		t.Fatal(err)
	}
	if err := worker.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.FindReplicaAsset("bob", "b1"); err != nil || found {
		t.Fatalf("work discovered a new origin: found=%v err=%v", found, err)
	}
	if err := worker.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.FindReplicaAsset("bob", "b1"); err != nil || !found {
		t.Fatalf("full discovery missed new origin: found=%v err=%v", found, err)
	}
	if measured.albumSearches == 0 {
		t.Fatal("full discovery performed no album search")
	}
}

func TestLargeDelayedImportUsesBoundedWorkAndCoalescedScans(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0750); err != nil {
		t.Fatal(err)
	}
	members := []domain.Member{{ID: "alice", UserID: "user-a", LibraryID: "lib-a"}, {ID: "bob", UserID: "user-b", LibraryID: "lib-b"}, {ID: "carol", UserID: "user-c", LibraryID: "lib-c"}}
	c := config.Config{FamilyID: "load", SourceRoot: source, SourceMappings: []config.SourceMapping{{ImmichRoot: source, LocalRoot: source}}, BridgeRoot: filepath.Join(root, "bridge"), ImmichBridgeRoot: "/bridge", Database: filepath.Join(root, "state", "bridge.sqlite"), Members: members}
	seed := fake.Seed{Albums: []fake.SeedAlbum{{ID: "load-album", Member: "alice", Name: "Load"}}}
	for i := 0; i < 520; i++ {
		id := fmt.Sprintf("source-%04d", i)
		path := filepath.Join(source, id+".jpg")
		if err := os.WriteFile(path, []byte(id), 0640); err != nil {
			t.Fatal(err)
		}
		seed.Assets = append(seed.Assets, fake.SeedAsset{ID: id, Member: "alice", Path: path})
		seed.Albums[0].AssetIDs = append(seed.Albums[0].AssetIDs, id)
	}
	db, err := store.Open(c.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Init(c.FamilyID, c.Members); err != nil {
		t.Fatal(err)
	}
	api, err := fake.New(c, filepath.Join(root, "fake.json"), seed)
	if err != nil {
		t.Fatal(err)
	}
	api.BlockScans(true)
	measured := &measuringClient{Client: api, scans: map[string]int{}}
	r := New(c, db, measured, slog.New(slog.NewTextHandler(io.Discard, nil)))
	clock := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return clock }
	ctx := context.Background()
	albumID, err := r.Register(ctx, "alice", "load-album")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if measured.scans["bob"] != 1 || measured.scans["carol"] != 1 {
		t.Fatalf("expected one scan per recipient during backlog: %+v", measured.scans)
	}
	replicas, err := db.Replicas()
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]int{}
	for _, rep := range replicas {
		states[rep.State]++
	}
	if states["pending_import"] != 1040 || states["ready"] != 520 {
		t.Fatalf("bounded preparation did not cover 520 assets: %+v", states)
	}
	api.BlockScans(false)
	// This test advances the fake clock by two minutes; bypass the production
	// stale-claim lease so it can exercise the next retry immediately.
	r.scanLease = 0
	clock = clock.Add(2 * time.Minute)
	for i := 0; i < 11; i++ {
		if err := r.Work(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if measured.scans["bob"] != 2 || measured.scans["carol"] != 2 {
		t.Fatalf("expected a single retry scan per recipient: %+v", measured.scans)
	}
	for _, member := range members {
		albumReps, err := db.AlbumReplicas(albumID)
		if err != nil {
			t.Fatal(err)
		}
		assets, err := api.ListAlbumAssets(ctx, member, albumReps[member.ID])
		if err != nil || len(assets) != 520 {
			t.Fatalf("member %s has %d assets: %v", member.ID, len(assets), err)
		}
	}
	batched := false
	for _, size := range measured.addSizes {
		if size > albumBatchSize {
			t.Fatalf("album add exceeded batch size: %d", size)
		}
		if size > 1 {
			batched = true
		}
	}
	if !batched {
		t.Fatal("album additions were not batched")
	}
}

func TestPersistentDryRunDoesNotWriteImmichOrMedia(t *testing.T) {
	c, db, api, _ := setup(t)
	c.DryRun = true
	r := New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	id, err := r.Register(ctx, "alice", "trip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal("repeat registration:", err)
	}
	reps, err := db.AlbumReplicas(id)
	if err != nil || len(reps) != 1 || reps["alice"] != "trip" {
		t.Fatalf("dry-run registration created remote albums: %+v, %v", reps, err)
	}
	preview, err := r.DryRun(ctx)
	if err != nil || len(preview) == 0 {
		t.Fatalf("missing dry-run actions: %+v, %v", preview, err)
	}
	var discovered, shared bool
	for _, action := range preview {
		if action.Kind == "discover_origin" && action.MemberID == "alice" && action.ImmichAssetID == "a1" {
			discovered = true
		}
		if action.Kind == "link_and_import" && action.SourceMemberID == "alice" && action.MemberID == "bob" && action.ImmichAssetID == "a1" {
			shared = true
		}
	}
	if !discovered || !shared {
		t.Fatalf("dry run did not identify the new source and recipient: %+v", preview)
	}
	if err := r.Run(ctx); !errors.Is(err, ErrDryRunMode) {
		t.Fatalf("active reconciliation in dry-run mode: %v", err)
	}
	assets, err := db.Replicas()
	if err != nil || len(assets) != 0 {
		t.Fatalf("dry run wrote asset replicas: %+v, %v", assets, err)
	}
	if _, err := os.Stat(c.BridgeRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created recipient tree: %v", err)
	}
	c.DryRun = false
	if err := New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx); err != nil {
		t.Fatal("active reconciliation after dry run:", err)
	}
	reps, err = db.AlbumReplicas(id)
	if err != nil || len(reps) != len(c.Members) {
		t.Fatalf("active reconciliation did not create albums: %+v, %v", reps, err)
	}
}

func setup(t *testing.T) (config.Config, *store.Store, *fake.Client, *Reconciler) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".familybridge-fixture"), []byte("familybridge-test-fixture-v1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	bridge := filepath.Join(root, "bridge")
	_ = os.MkdirAll(source, 0750)
	aPath := filepath.Join(source, "a.jpg")
	bPath := filepath.Join(source, "b.jpg")
	_ = os.WriteFile(aPath, []byte("photo a"), 0640)
	_ = os.WriteFile(bPath, []byte("photo b"), 0640)
	c := config.Config{FamilyID: "family", TogetherAlbumName: "Together", SourceRoot: source, SourceMappings: []config.SourceMapping{{ImmichRoot: source, LocalRoot: source}}, BridgeRoot: bridge, ImmichBridgeRoot: "/immich-bridge", Database: filepath.Join(root, "state", "db.sqlite"), APITokenEnv: "TEST_TOKEN", APIToken: "test", PollInterval: "30s", Members: []domain.Member{{ID: "alice", UserID: "user-a", LibraryID: "lib-a"}, {ID: "bob", UserID: "user-b", LibraryID: "lib-b"}, {ID: "carol", UserID: "user-c", LibraryID: "lib-c"}}}
	seed := fake.Seed{Assets: []fake.SeedAsset{{ID: "a1", Member: "alice", Path: aPath}, {ID: "b1", Member: "bob", Path: bPath}}, Albums: []fake.SeedAlbum{{ID: "trip", Member: "alice", Name: "Trip", AssetIDs: []string{"a1"}}, {ID: "best", Member: "alice", Name: "Best", AssetIDs: []string{"a1"}}}}
	db, e := store.Open(c.Database)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	if e := db.Init(c.FamilyID, c.Members); e != nil {
		t.Fatal(e)
	}
	api, e := fake.New(c, filepath.Join(root, "state", "fake.json"), seed)
	if e != nil {
		t.Fatal(e)
	}
	r := New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c, db, api, r
}

func TestLateSidecarIsLinkedDiscoveredAndRefreshed(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	lid, found, err := db.FindReplicaAsset("alice", "a1")
	if err != nil || !found {
		t.Fatalf("origin mapping: %q %v", lid, err)
	}
	sidecar := filepath.Join(c.SourceRoot, "a.jpg.xmp")
	if err := os.WriteFile(sidecar, []byte("<xmp>metadata</xmp>"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := api.SetSidecar("a1", sidecar); err != nil {
		t.Fatal(err)
	}
	measured := &measuringClient{Client: api, scans: map[string]int{}, metadataRefreshes: map[string]int{}}
	r = New(c, db, measured, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.scanInterval = 0
	if err := r.Run(ctx); err != nil {
		t.Fatal("link and discovery cycle:", err)
	}
	for _, memberID := range []string{"bob", "carol"} {
		replica, ok, err := db.Replica(lid, memberID)
		if err != nil || !ok {
			t.Fatalf("recipient %s: %+v %v", memberID, replica, err)
		}
		files, err := db.ReplicaFiles(lid, memberID)
		if err != nil || files["sidecar"].State != "pending_discovery" {
			t.Fatalf("sidecar should await Immich discovery for %s: %+v %v", memberID, files, err)
		}
		sourceInfo, _ := os.Stat(sidecar)
		recipientInfo, statErr := os.Stat(replica.Path + ".xmp")
		if statErr != nil || !os.SameFile(sourceInfo, recipientInfo) {
			t.Fatalf("sidecar for %s is not a hardlink: %v", memberID, statErr)
		}
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal("association and metadata refresh cycle:", err)
	}
	for _, memberID := range []string{"bob", "carol"} {
		files, err := db.ReplicaFiles(lid, memberID)
		if err != nil || files["sidecar"].State != "ready" {
			t.Fatalf("sidecar did not become ready for %s: %+v %v", memberID, files, err)
		}
		if measured.metadataRefreshes[memberID] != 0 {
			t.Fatalf("metadata refreshes for %s = %d", memberID, measured.metadataRefreshes[memberID])
		}
	}
	if err := os.WriteFile(sidecar, []byte("<xmp>updated metadata with a different size</xmp>"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal("sidecar update cycle:", err)
	}
	if measured.metadataRefreshes["bob"] != 1 || measured.metadataRefreshes["carol"] != 1 {
		t.Fatalf("updated sidecars were not refreshed once: %+v", measured.metadataRefreshes)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal("idempotent cycle:", err)
	}
	if measured.metadataRefreshes["bob"] != 1 || measured.metadataRefreshes["carol"] != 1 {
		t.Fatalf("unchanged sidecars refreshed again: %+v", measured.metadataRefreshes)
	}
}

func TestTogetherIsProvisionedAfterDryRunWithoutDuplicates(t *testing.T) {
	c, db, api, _ := setup(t)
	c.DryRun = true
	ctx := context.Background()
	previewer := New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id, err := previewer.EnsureTogether(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := previewer.EnsureTogether(ctx); err != nil || again != id {
		t.Fatalf("Together was not stable across retries: %q, %v", again, err)
	}
	actions, err := previewer.DryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	creates := 0
	for _, action := range actions {
		if action.Kind == "create_album_replica" && action.AlbumID == id {
			creates++
		}
	}
	if creates != len(c.Members) {
		t.Fatalf("dry-run did not preview one album per member: %+v", actions)
	}
	for _, m := range c.Members {
		albums, err := api.ListAlbums(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		for _, album := range albums {
			if album.Name == "Together" {
				t.Fatal("dry-run created an Immich album")
			}
		}
	}
	later, err := api.CreateAlbum(ctx, c.Members[0], "created-after-preview", "Together", "")
	if err != nil {
		t.Fatal(err)
	}
	c.DryRun = false
	active := New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := active.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := active.Run(ctx); err != nil {
		t.Fatal("repeat active cycle:", err)
	}
	if again, err := active.EnsureTogether(ctx); err != nil || again != id {
		t.Fatalf("Together changed after activation: %q, %v", again, err)
	}
	replicas, err := db.AlbumReplicas(id)
	if err != nil || replicas["alice"] != later.ID {
		t.Fatalf("existing member album was not reused: %+v, %v", replicas, err)
	}
	stored, err := db.Albums()
	if err != nil || len(stored) != 1 || stored[0].SystemKey != "together" {
		t.Fatalf("unexpected logical albums: %+v, %v", stored, err)
	}
	for _, m := range c.Members {
		albums, err := api.ListAlbums(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, album := range albums {
			if album.Name == "Together" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("member %s has %d Together albums", m.ID, count)
		}
	}
}

func TestTogetherAdoptsExistingAlbumsAndUsesConfiguredName(t *testing.T) {
	c, db, api, _ := setup(t)
	c.TogetherAlbumName = "Family Room"
	c.DryRun = true
	ctx := context.Background()
	a, err := api.CreateAlbum(ctx, c.Members[0], "existing-a", "Family Room", "Alice's description")
	if err != nil {
		t.Fatal(err)
	}
	b, err := api.CreateAlbum(ctx, c.Members[1], "existing-b", "Family Room", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.AddAssets(ctx, c.Members[0], a.ID, []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	if err := api.AddAssets(ctx, c.Members[1], b.ID, []string{"b1"}); err != nil {
		t.Fatal(err)
	}
	previewer := New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manualID, err := previewer.Register(ctx, "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := previewer.EnsureTogether(ctx)
	if err != nil || id != manualID {
		t.Fatalf("manual Together album was not adopted: %q, %v", id, err)
	}
	reps, err := db.AlbumReplicas(id)
	if err != nil || reps["alice"] != a.ID || reps["bob"] != b.ID || reps["carol"] != "" {
		t.Fatalf("existing albums not attached: %+v, %v", reps, err)
	}
	c.DryRun = false
	active := New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := active.Run(ctx); err != nil {
		t.Fatal(err)
	}
	reps, err = db.AlbumReplicas(id)
	if err != nil || len(reps) != len(c.Members) {
		t.Fatalf("missing member album: %+v, %v", reps, err)
	}
	for _, m := range c.Members {
		assets, err := api.ListAlbumAssets(ctx, m, reps[m.ID])
		if err != nil || len(assets) != 2 {
			t.Fatalf("member %s did not receive union: %+v, %v", m.ID, assets, err)
		}
		album, err := api.GetAlbum(ctx, m, reps[m.ID])
		if err != nil || album.Name != "Family Room" {
			t.Fatalf("member %s has wrong configured album name: %+v, %v", m.ID, album, err)
		}
	}
}

func TestTogetherRejectsAmbiguousExistingName(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	for _, id := range []string{"first", "second"} {
		if _, err := api.CreateAlbum(ctx, c.Members[1], id, "Together", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.EnsureTogether(ctx); err == nil {
		t.Fatal("ambiguous member albums were accepted")
	}
	albums, err := db.Albums()
	if err != nil || len(albums) != 0 {
		t.Fatalf("ambiguous discovery wrote a logical album: %+v, %v", albums, err)
	}
}

func TestNUserAlbumFlowRestartAndReferences(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	trip, e := r.Register(ctx, "alice", "trip")
	if e != nil {
		t.Fatal(e)
	}
	best, e := r.Register(ctx, "alice", "best")
	if e != nil {
		t.Fatal(e)
	}
	preview, e := r.DryRun(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if len(preview) == 0 {
		t.Fatal("dry run missed initial work")
	}
	before, _ := db.Replicas()
	if len(before) != 0 {
		t.Fatal("dry run wrote asset mappings")
	}
	if e := r.Run(ctx); e != nil {
		t.Fatal(e)
	}
	if e := r.Run(ctx); e != nil {
		t.Fatal("not idempotent:", e)
	}
	for _, m := range c.Members {
		reps, _ := db.AlbumReplicas(trip)
		assets, e := api.ListAlbumAssets(ctx, m, reps[m.ID])
		if e != nil || len(assets) != 1 {
			t.Fatalf("%s assets %v: %v", m.ID, assets, e)
		}
	}
	lid, ok, e := db.FindReplicaAsset("alice", "a1")
	if e != nil || !ok {
		t.Fatal(e)
	}
	count, _ := db.SourceCount(lid)
	if e := r.UpdateAlbum(trip, "New Trip", "Updated", lid); e != nil {
		t.Fatal(e)
	}
	if e := r.Run(ctx); e != nil {
		t.Fatal(e)
	}
	tripRepsForCover, _ := db.AlbumReplicas(trip)
	for _, m := range c.Members {
		remote, e := api.GetAlbum(ctx, m, tripRepsForCover[m.ID])
		if e != nil {
			t.Fatal(e)
		}
		rep, _, _ := db.Replica(lid, m.ID)
		if remote.Name != "New Trip" || remote.Description != "Updated" || remote.CoverID != rep.AssetID {
			t.Fatalf("metadata for %s: %+v", m.ID, remote)
		}
	}
	if count != 2 {
		t.Fatalf("want 2 sharing sources, got %d", count)
	}
	if e := api.RemoveAssets(ctx, c.Members[0], "trip", []string{"a1"}); e != nil {
		t.Fatal(e)
	}
	if e := r.Run(ctx); e != nil {
		t.Fatal(e)
	}
	count, _ = db.SourceCount(lid)
	if count != 1 {
		t.Fatalf("want 1 source, got %d", count)
	}
	rep, _, _ := db.Replica(lid, "bob")
	if rep.State != "ready" {
		t.Fatalf("replica lost despite second album: %+v", rep)
	}
	tripReps, _ := db.AlbumReplicas(trip)
	api.BlockScans(true)
	var pendingLogs bytes.Buffer
	r.Log = slog.New(slog.NewJSONHandler(&pendingLogs, nil))
	if e := api.AddAssets(ctx, c.Members[1], tripReps["bob"], []string{"b1"}); e != nil {
		t.Fatal(e)
	}
	if e := r.Run(ctx); e != nil {
		t.Fatal(e)
	}
	blid, ok, _ := db.FindReplicaAsset("bob", "b1")
	if !ok {
		t.Fatal("B source not mapped")
	}
	br, ok, _ := db.Replica(blid, "carol")
	if !ok || br.State != "pending_import" {
		t.Fatalf("want pending import: %+v", br)
	}
	if got := pendingLogs.String(); !strings.Contains(got, `"pending_import":1`) || strings.Contains(got, `"level":"WARN"`) || strings.Contains(got, `"error":`) {
		t.Fatalf("expected import wait to be informational: %s", got)
	}
	api.BlockScans(false)
	r.scanInterval = 0
	if e := r.Run(ctx); e != nil {
		t.Fatal(e)
	}
	br, _, _ = db.Replica(blid, "carol")
	if br.State != "ready" || br.AssetID == "" {
		t.Fatalf("import not recovered: %+v", br)
	}
	api.FailNext()
	if e := r.Run(ctx); e == nil {
		t.Fatal("transient outage ignored")
	}
	if e := r.Run(ctx); e != nil {
		t.Fatal("retry failed:", e)
	}
	api2, e := fake.New(c, filepath.Join(filepath.Dir(c.Database), "fake.json"), fake.Seed{})
	if e != nil {
		t.Fatal(e)
	}
	r2 := New(c, db, api2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Disable the production scan lease so this unit test can force an immediate retry.
	r2.scanInterval = 0
	if e := r2.Run(ctx); e != nil {
		t.Fatal("restart failed:", e)
	}
	if e := api2.DeleteAsset(br.AssetID); e != nil {
		t.Fatal(e)
	}
	if e := r2.Run(ctx); e != nil {
		t.Fatal("manual replica deletion recovery:", e)
	}
	br, _, _ = db.Replica(blid, "carol")
	if br.State != "ready" || br.AssetID == "" {
		t.Fatalf("manual deletion not repaired: %+v", br)
	}
	if e := api2.RemoveAssets(ctx, c.Members[0], "best", []string{"a1"}); e != nil {
		t.Fatal(e)
	}
	if e := r2.Run(ctx); e != nil {
		t.Fatal(e)
	}
	count, _ = db.SourceCount(lid)
	if count != 0 {
		t.Fatalf("want no sources, got %d", count)
	}
	rep, _, _ = db.Replica(lid, "bob")
	if rep.State != "pending_removal" {
		t.Fatalf("want pending removal: %+v", rep)
	}
	if _, e := os.Stat(rep.Path); e != nil {
		t.Fatal("file deleted:", e)
	}
	_ = best
}

func TestRegisterExistingAlbumsTakesUnion(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	bAlbum, e := api.CreateAlbum(ctx, c.Members[1], "existing-b-trip", "Trip", "")
	if e != nil {
		t.Fatal(e)
	}
	if e := api.AddAssets(ctx, c.Members[1], bAlbum.ID, []string{"b1"}); e != nil {
		t.Fatal(e)
	}
	logical, e := r.RegisterWithReplicas(ctx, "alice", "trip", map[string]string{"bob": bAlbum.ID})
	if e != nil {
		t.Fatal(e)
	}
	if e := r.Run(ctx); e != nil {
		t.Fatal(e)
	}
	reps, _ := db.AlbumReplicas(logical)
	for _, m := range c.Members {
		assets, e := api.ListAlbumAssets(ctx, m, reps[m.ID])
		if e != nil || len(assets) != 2 {
			t.Fatalf("%s union: %#v %v", m.ID, assets, e)
		}
	}
	var count int
	if e := db.DB.QueryRow(`SELECT COUNT(*) FROM logical_assets`).Scan(&count); e != nil || count != 2 {
		t.Fatalf("feedback loop: %d %v", count, e)
	}
}

func TestRemoveLastSharingReferenceDeletesOnlyRecipientReplicas(t *testing.T) {
	c, db, api, r := setup(t)
	c.RemoveUnshared = true
	r = New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	sidecar := filepath.Join(c.SourceRoot, "a.jpg.xmp")
	if err := os.WriteFile(sidecar, []byte("xmp"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := api.SetSidecar("a1", sidecar); err != nil {
		t.Fatal(err)
	}
	album, err := r.Register(ctx, "alice", "trip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	lid, ok, err := db.FindReplicaAsset("alice", "a1")
	if err != nil || !ok {
		t.Fatal("origin mapping:", err)
	}
	replicas, err := db.ReplicasFor(lid)
	if err != nil || len(replicas) != 3 {
		t.Fatalf("initial replicas: %+v, %v", replicas, err)
	}
	recipientPaths := []string{}
	for _, replica := range replicas {
		if replica.Role != "external_replica" {
			continue
		}
		files, err := db.ReplicaFiles(lid, replica.MemberID)
		if err != nil || len(files) != 2 {
			t.Fatalf("recipient components for %s: %+v, %v", replica.MemberID, files, err)
		}
		for _, component := range files {
			recipientPaths = append(recipientPaths, component.RecipientPath)
			if _, err := os.Stat(component.RecipientPath); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := api.RemoveAssets(ctx, c.Members[0], "trip", []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	dryConfig := c
	dryConfig.DryRun = true
	preview, err := New(dryConfig, db, api, slog.New(slog.NewTextHandler(io.Discard, nil))).DryRun(ctx)
	if err != nil {
		t.Fatal("cleanup dry run:", err)
	}
	deleteActions := 0
	for _, action := range preview {
		if action.Kind == "delete_recipient_replica" && action.LogicalAssetID == lid {
			deleteActions++
		}
	}
	if deleteActions != 2 {
		t.Fatalf("cleanup dry run reported %d recipient deletions: %+v", deleteActions, preview)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal("mark unused:", err)
	}
	for _, member := range []string{"bob", "carol"} {
		replica, found, err := db.Replica(lid, member)
		if err != nil || !found || replica.State != "pending_removal" {
			t.Fatalf("%s was not given a grace cycle: %+v, %v", member, replica, err)
		}
	}
	for _, path := range recipientPaths {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("grace cycle removed file:", err)
		}
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal("request deletion:", err)
	}
	for _, member := range []string{"bob", "carol"} {
		replica, found, err := db.Replica(lid, member)
		if err != nil || !found || replica.State != "deleting" {
			t.Fatalf("%s deletion state was not persisted: %+v, %v", member, replica, err)
		}
	}
	// A new reconciler models a process crash after Immich accepted deletion but
	// before local files and mappings were finalized.
	r = New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := r.Run(ctx); err != nil {
		t.Fatal("restart cleanup:", err)
	}
	for _, member := range []string{"bob", "carol"} {
		if replica, found, err := db.Replica(lid, member); err != nil || found {
			t.Fatalf("%s recipient mapping remains: %+v, %v", member, replica, err)
		}
	}
	for _, path := range recipientPaths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recipient file remains at %s: %v", path, err)
		}
	}
	for _, path := range []string{filepath.Join(c.SourceRoot, "a.jpg"), sidecar} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("source was removed at %s: %v", path, err)
		}
	}
	origin, found, err := db.Replica(lid, "alice")
	if err != nil || !found || origin.AssetID != "a1" || origin.State != "ready" {
		t.Fatalf("origin mapping changed: %+v, %v", origin, err)
	}
	albumReplicas, _ := db.AlbumReplicas(album)
	for _, member := range c.Members {
		assets, err := api.ListAlbumAssets(ctx, member, albumReplicas[member.ID])
		if err != nil || len(assets) != 0 {
			t.Fatalf("%s album still contains assets: %+v, %v", member.ID, assets, err)
		}
	}
}

func TestRemovalWaitsForAnInFlightImportBeforeDeleting(t *testing.T) {
	c, db, api, _ := setup(t)
	c.RemoveUnshared = true
	r := New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.scanInterval = 0
	ctx := context.Background()
	api.BlockScans(true)
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	lid, ok, err := db.FindReplicaAsset("alice", "a1")
	if err != nil || !ok {
		t.Fatal("origin mapping:", err)
	}
	bob, found, err := db.Replica(lid, "bob")
	if err != nil || !found || bob.State != "pending_import" || bob.AssetID != "" {
		t.Fatalf("initial pending import: %+v, %v", bob, err)
	}
	if err := api.RemoveAssets(ctx, c.Members[0], "trip", []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	bob, found, err = db.Replica(lid, "bob")
	if err != nil || !found || bob.State != "pending_removal" {
		t.Fatalf("in-flight import was not retained: %+v, %v", bob, err)
	}
	if _, err := os.Stat(bob.Path); err != nil {
		t.Fatal("pending hardlink was removed:", err)
	}
	api.BlockScans(false)
	for i := 0; i < 3; i++ {
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if bob, found, err := db.Replica(lid, "bob"); err != nil || found {
		t.Fatalf("recipient remains after import and delete: %+v, %v", bob, err)
	}
	if _, err := os.Stat(filepath.Join(c.SourceRoot, "a.jpg")); err != nil {
		t.Fatal("source was removed:", err)
	}
}

func TestMissingSourceRetainsLinks(t *testing.T) {
	_, db, api, r := setup(t)
	ctx := context.Background()
	album, e := r.Register(ctx, "alice", "trip")
	if e != nil {
		t.Fatal(e)
	}
	if e := r.Run(ctx); e != nil {
		t.Fatal(e)
	}
	lid, _, _ := db.FindReplicaAsset("alice", "a1")
	rep, _, _ := db.Replica(lid, "bob")
	if e := api.DeleteAsset("a1"); e != nil {
		t.Fatal(e)
	}
	if e := r.Run(ctx); e != nil {
		t.Fatal(e)
	}
	refs, _ := db.SourceCount(lid)
	if refs != 1 {
		t.Fatalf("source deletion revoked sharing: %d", refs)
	}
	rep, _, _ = db.Replica(lid, "bob")
	if rep.State != "source_missing" {
		t.Fatalf("missing source state: %+v", rep)
	}
	if _, e := os.Stat(rep.Path); e != nil {
		t.Fatal("recipient link removed:", e)
	}
	_ = album
}

func TestOriginPathMoveRequiresSameInode(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	lid, _, _ := db.FindReplicaAsset("alice", "a1")
	origin, _, _ := db.Replica(lid, "alice")
	recipient, _, _ := db.Replica(lid, "bob")
	moved := filepath.Join(c.SourceRoot, "moved.jpg")
	if err := os.Rename(origin.Path, moved); err != nil {
		t.Fatal(err)
	}
	if err := api.MoveAssetPath("a1", moved); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	origin, _, _ = db.Replica(lid, "alice")
	if origin.Path != moved {
		t.Fatalf("safe same-inode move not recorded: %+v", origin)
	}
	a, _ := os.Stat(moved)
	b, _ := os.Stat(recipient.Path)
	if !os.SameFile(a, b) {
		t.Fatal("recipient changed inode after source move")
	}
	different := filepath.Join(c.SourceRoot, "different.jpg")
	if err := os.WriteFile(different, []byte("different inode"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := api.MoveAssetPath("a1", different); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	origin, _, _ = db.Replica(lid, "alice")
	if origin.Path != moved {
		t.Fatalf("different inode replaced origin path: %+v", origin)
	}
}
