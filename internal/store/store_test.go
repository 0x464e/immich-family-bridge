package store

import (
	"path/filepath"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/domain"
)

func TestPersistenceAndMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.sqlite")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	members := []domain.Member{{ID: "a", UserID: "u-a", LibraryID: "l-a"}, {ID: "b", UserID: "u-b", LibraryID: "l-b"}}
	if e := s.Init("family", members); e != nil {
		t.Fatal(e)
	}
	id, e := s.EnsureOrigin("family", "a", domain.Asset{ID: "asset-a", OwnerID: "u-a", OriginalPath: "/fixture/a.jpg"})
	if e != nil {
		t.Fatal(e)
	}
	if e := s.UpsertReplica(domain.Replica{LogicalID: id, MemberID: "b", Role: "external_replica", Path: "/fixture/bridge/b.jpg", LibraryID: "l-b", State: "pending_import"}); e != nil {
		t.Fatal(e)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e := s.Init("family", members); e != nil {
		t.Fatal(e)
	}
	rep, found, e := s.Replica(id, "b")
	if e != nil || !found || rep.State != "pending_import" {
		t.Fatalf("mapping lost on restart: %+v %v", rep, e)
	}
	var version int
	if e := s.DB.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); e != nil || version != 2 {
		t.Fatalf("migration version %d %v", version, e)
	}
	if _, e := s.DB.Exec(`INSERT INTO asset_replica_files(logical_asset_id,member_id,component_kind,source_path,recipient_path,state) VALUES(?,?,?,?,?,?)`, id, "b", "original", "/fixture/a.jpg", "/fixture/bridge/b.jpg", "ready"); e != nil {
		t.Fatal(e)
	}
}
