package store

import (
	"database/sql"
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
	var synchronous int
	if e := s.DB.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); e != nil || synchronous != 2 {
		t.Fatalf("SQLite synchronous mode = %d, want FULL (2): %v", synchronous, e)
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
	if e := s.DB.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); e != nil || version != 4 {
		t.Fatalf("migration version %d %v", version, e)
	}
	if _, e := s.DB.Exec(`INSERT INTO asset_replica_files(logical_asset_id,member_id,component_kind,source_path,recipient_path,state) VALUES(?,?,?,?,?,?)`, id, "b", "original", "/fixture/a.jpg", "/fixture/bridge/b.jpg", "ready"); e != nil {
		t.Fatal(e)
	}
}

func TestUpgradeExistingDatabasePreservesAlbums(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for i, file := range []string{"migrations/001_init.sql", "migrations/002_components.sql"} {
		content, err := migrations.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(content)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version) VALUES(?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO families(id) VALUES('family'); INSERT INTO logical_albums(id,family_id,name,description) VALUES('old-together','family','Together','existing album')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	albums, err := store.Albums()
	if err != nil || len(albums) != 1 || albums[0].ID != "old-together" || albums[0].SystemKey != "" {
		t.Fatalf("existing album changed during migration: %+v, %v", albums, err)
	}
	if err := store.SetAlbumSystemKey("old-together", "together"); err != nil {
		t.Fatal(err)
	}
	albums, err = store.Albums()
	if err != nil || albums[0].SystemKey != "together" {
		t.Fatalf("system key not persisted: %+v, %v", albums, err)
	}
}
