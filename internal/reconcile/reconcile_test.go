package reconcile

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/config"
	"github.com/0x464e/immich-family-bridge/internal/domain"
	fake "github.com/0x464e/immich-family-bridge/internal/immich/testfake"
	"github.com/0x464e/immich-family-bridge/internal/store"
)

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
	c := config.Config{FamilyID: "family", SourceRoot: source, SourceMappings: []config.SourceMapping{{ImmichRoot: source, LocalRoot: source}}, BridgeRoot: bridge, ImmichBridgeRoot: "/immich-bridge", Database: filepath.Join(root, "state", "db.sqlite"), APITokenEnv: "TEST_TOKEN", APIToken: "test", PollInterval: "30s", Members: []domain.Member{{ID: "alice", UserID: "user-a", LibraryID: "lib-a"}, {ID: "bob", UserID: "user-b", LibraryID: "lib-b"}, {ID: "carol", UserID: "user-c", LibraryID: "lib-c"}}}
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
	if preview["count"].(int) == 0 {
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
	api.BlockScans(false)
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
