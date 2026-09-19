package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db}
	if err = s.Migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

//go:embed migrations/*.sql
var migrations embed.FS

func (s *Store) Migrate() error {
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	files := []string{"migrations/001_init.sql", "migrations/002_components.sql", "migrations/003_system_albums.sql", "migrations/004_component_signatures.sql", "migrations/005_library_scan_state.sql", "migrations/006_album_cover_observations.sql"}
	var max int
	if err := s.DB.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&max); err != nil {
		return err
	}
	if max > len(files) {
		return fmt.Errorf("database schema version %d is newer than application", max)
	}
	for i, file := range files {
		version := i + 1
		if version <= max {
			continue
		}
		sqlText, err := migrations.ReadFile(file)
		if err != nil {
			return err
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(string(sqlText)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err = tx.Exec(`INSERT INTO schema_migrations(version) VALUES(?)`, version); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ClaimLibraryScan atomically reserves a library scan for a member. A new
// claim is allowed when the previous scan's observed refresh timestamp
// changed, or when the lease interval has elapsed. The timestamp and lease
// are persisted so separate bridge processes and restarts cannot immediately
// enqueue overlapping scans.
func (s *Store) ClaimLibraryScan(memberID string, requestedAt time.Time, baselineRefreshedAt string, lease time.Duration) (bool, error) {
	result, err := s.DB.Exec(`
		INSERT INTO library_scan_state(member_id, requested_at_ns, baseline_refreshed_at)
		VALUES(?,?,?)
		ON CONFLICT(member_id) DO UPDATE SET
			requested_at_ns=excluded.requested_at_ns,
			baseline_refreshed_at=excluded.baseline_refreshed_at
		WHERE (excluded.baseline_refreshed_at != '' AND library_scan_state.baseline_refreshed_at != excluded.baseline_refreshed_at)
		   OR excluded.requested_at_ns - library_scan_state.requested_at_ns >= ?`,
		memberID, requestedAt.UnixNano(), baselineRefreshedAt, lease.Nanoseconds())
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// ReleaseLibraryScan removes a failed claim without deleting a newer claim
// made by another process.
func (s *Store) ReleaseLibraryScan(memberID string, requestedAt time.Time) error {
	_, err := s.DB.Exec(`DELETE FROM library_scan_state WHERE member_id=? AND requested_at_ns=?`, memberID, requestedAt.UnixNano())
	return err
}

func ID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

func (s *Store) Init(family string, members []domain.Member) error {
	ctx := context.Background()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingFamily string
	if e := tx.QueryRow(`SELECT id FROM families LIMIT 1`).Scan(&existingFamily); e == nil && existingFamily != family {
		return errors.New("database belongs to another family")
	} else if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO families(id) VALUES(?)`, family); err != nil {
		return err
	}
	for _, m := range members {
		if _, err = tx.Exec(`INSERT OR IGNORE INTO members(id,family_id,user_id,library_id) VALUES(?,?,?,?)`, m.ID, family, m.UserID, m.LibraryID); err != nil {
			return err
		}
		var u, l string
		if err = tx.QueryRow(`SELECT user_id,library_id FROM members WHERE id=?`, m.ID).Scan(&u, &l); err != nil {
			return err
		}
		if u != m.UserID || l != m.LibraryID {
			return fmt.Errorf("member %s identity changed; migration required", m.ID)
		}
	}
	var count int
	if e := tx.QueryRow(`SELECT COUNT(*) FROM members`).Scan(&count); e != nil {
		return e
	}
	if count != len(members) {
		return errors.New("configured members do not match persisted members; explicit migration required")
	}
	return tx.Commit()
}

func (s *Store) AddAlbum(family string, a domain.LogicalAlbum, replicas map[string]string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var systemKey any
	if a.SystemKey != "" {
		systemKey = a.SystemKey
	}
	_, err = tx.Exec(`INSERT INTO logical_albums(id,family_id,name,description,system_key) VALUES(?,?,?,?,?)`, a.ID, family, a.Name, a.Description, systemKey)
	if err != nil {
		return err
	}
	for member, id := range replicas {
		_, err = tx.Exec(`INSERT INTO album_replicas(logical_album_id,member_id,immich_album_id,state) VALUES(?,?,?,'ready')`, a.ID, member, id)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AlbumByReplica(member, immichID string) (string, bool, error) {
	var id string
	err := s.DB.QueryRow(`SELECT logical_album_id FROM album_replicas WHERE member_id=? AND immich_album_id=?`, member, immichID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}
func (s *Store) SetCover(album, logical string) error {
	var value any
	if logical != "" {
		value = logical
	}
	_, err := s.DB.Exec(`UPDATE logical_albums SET cover_logical_asset_id=? WHERE id=?`, value, album)
	return err
}

// AlbumCoverObservation returns the last cover successfully observed or
// written by the bridge for one member's album replica.
func (s *Store) AlbumCoverObservation(album, member string) (string, bool, error) {
	var cover string
	err := s.DB.QueryRow(`SELECT immich_asset_id FROM album_cover_observations WHERE logical_album_id=? AND member_id=?`, album, member).Scan(&cover)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return cover, err == nil, err
}

// SetAlbumCoverObservation records a cover only after the matching Immich
// album update has succeeded. This distinguishes a user edit from a bridge
// retry after a partial failure.
func (s *Store) SetAlbumCoverObservation(album, member, cover string) error {
	_, err := s.DB.Exec(`INSERT INTO album_cover_observations(logical_album_id,member_id,immich_asset_id)
		VALUES(?,?,?)
		ON CONFLICT(logical_album_id,member_id) DO UPDATE SET immich_asset_id=excluded.immich_asset_id`, album, member, cover)
	return err
}

func (s *Store) SetAlbumSystemKey(album, key string) error {
	result, err := s.DB.Exec(`UPDATE logical_albums SET system_key=? WHERE id=? AND (system_key IS NULL OR system_key=?)`, key, album, key)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("album system key conflicts with existing mapping")
	}
	return nil
}

func (s *Store) UpdateOriginPath(logicalID, memberID, oldPath, newPath string) error {
	result, err := s.DB.Exec(`UPDATE asset_replicas SET filesystem_path=? WHERE logical_asset_id=? AND member_id=? AND role='origin' AND filesystem_path=?`, newPath, logicalID, memberID, oldPath)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("origin path changed concurrently")
	}
	return nil
}

func (s *Store) UpdateAlbum(id, name, description, cover string) error {
	var cv any
	if cover != "" {
		cv = cover
	}
	result, err := s.DB.Exec(`UPDATE logical_albums SET name=?,description=?,cover_logical_asset_id=? WHERE id=?`, name, description, cv, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) Albums() ([]domain.LogicalAlbum, error) {
	r, err := s.DB.Query(`SELECT id,name,description,COALESCE(cover_logical_asset_id,''),initialized,COALESCE(system_key,'') FROM logical_albums ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := []domain.LogicalAlbum{}
	for r.Next() {
		var a domain.LogicalAlbum
		var init int
		if err := r.Scan(&a.ID, &a.Name, &a.Description, &a.CoverID, &init, &a.SystemKey); err != nil {
			return nil, err
		}
		a.Initialized = init != 0
		out = append(out, a)
	}
	return out, r.Err()
}

func (s *Store) AlbumReplicas(album string) (map[string]string, error) {
	r, err := s.DB.Query(`SELECT member_id,COALESCE(immich_album_id,'') FROM album_replicas WHERE logical_album_id=?`, album)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]string{}
	for r.Next() {
		var m, id string
		if err := r.Scan(&m, &id); err != nil {
			return nil, err
		}
		out[m] = id
	}
	return out, r.Err()
}

func (s *Store) SetAlbumReplica(album, member, id string) error {
	_, err := s.DB.Exec(`INSERT INTO album_replicas(logical_album_id,member_id,immich_album_id,state) VALUES(?,?,?,'ready') ON CONFLICT(logical_album_id,member_id) DO UPDATE SET immich_album_id=excluded.immich_album_id,state='ready'`, album, member, id)
	return err
}

func (s *Store) FindReplicaAsset(member, asset string) (string, bool, error) {
	var id string
	err := s.DB.QueryRow(`SELECT logical_asset_id FROM asset_replicas WHERE member_id=? AND immich_asset_id=?`, member, asset).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// ResolveActiveReplica finds the ready asset replica for an authenticated
// Immich user. A logical asset is resolvable only while it is actively shared;
// pending-removal replicas must retain Immich's ordinary access behaviour.
func (s *Store) ResolveActiveReplica(ctx context.Context, requestedAssetID, userID string) (string, bool, error) {
	var target string
	err := s.DB.QueryRowContext(ctx, `
		SELECT target.immich_asset_id
		FROM asset_replicas AS requested
		JOIN asset_replicas AS target ON target.logical_asset_id=requested.logical_asset_id
		JOIN members AS target_member ON target_member.id=target.member_id
		WHERE requested.immich_asset_id=?
		  AND target_member.user_id=?
		  AND target.immich_asset_id IS NOT NULL
		  AND target.immich_asset_id != ''
		  AND target.state='ready'
		  AND EXISTS (SELECT 1 FROM sharing_sources WHERE logical_asset_id=requested.logical_asset_id)
		LIMIT 1`, requestedAssetID, userID).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return target, err == nil, err
}

// ResolveMirrorAlbum finds the ready bridge-managed album replica for an
// authenticated Immich user. Only IDs recorded in album_replicas are
// resolvable, so ordinary Immich albums always keep their normal behaviour.
func (s *Store) ResolveMirrorAlbum(ctx context.Context, requestedAlbumID, userID string) (string, bool, error) {
	var target string
	err := s.DB.QueryRowContext(ctx, `
		SELECT target.immich_album_id
		FROM album_replicas AS requested
		JOIN album_replicas AS target ON target.logical_album_id=requested.logical_album_id
		JOIN members AS target_member ON target_member.id=target.member_id
		WHERE requested.immich_album_id=?
		  AND target_member.user_id=?
		  AND target.immich_album_id IS NOT NULL
		  AND target.immich_album_id != ''
		  AND target.state='ready'
		LIMIT 1`, requestedAlbumID, userID).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return target, err == nil, err
}

func (s *Store) EnsureOrigin(family, member string, a domain.Asset) (string, error) {
	var id string
	err := s.DB.QueryRow(`SELECT id FROM logical_assets WHERE origin_immich_asset_id=?`, a.ID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = ID()
	tx, err := s.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT OR IGNORE INTO logical_assets(id,family_id,origin_member_id,origin_immich_asset_id,created_at) VALUES(?,?,?,?,?)`, id, family, member, a.ID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	if err = tx.QueryRow(`SELECT id FROM logical_assets WHERE origin_immich_asset_id=?`, a.ID).Scan(&id); err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO asset_replicas(logical_asset_id,member_id,immich_asset_id,role,filesystem_path,state) VALUES(?,?,?,'origin',?,'ready')`, id, member, a.ID, a.OriginalPath)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *Store) LogicalAsset(id string) (domain.LogicalAsset, error) {
	var a domain.LogicalAsset
	err := s.DB.QueryRow(`SELECT id,origin_member_id,origin_immich_asset_id FROM logical_assets WHERE id=?`, id).Scan(&a.ID, &a.OriginMember, &a.OriginAsset)
	return a, err
}

func (s *Store) Replica(logical, member string) (domain.Replica, bool, error) {
	var r domain.Replica
	var aid, path, lib, er sql.NullString
	err := s.DB.QueryRow(`SELECT logical_asset_id,member_id,immich_asset_id,role,filesystem_path,external_library_id,state,error FROM asset_replicas WHERE logical_asset_id=? AND member_id=?`, logical, member).Scan(&r.LogicalID, &r.MemberID, &aid, &r.Role, &path, &lib, &r.State, &er)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.AssetID = aid.String
	r.Path = path.String
	r.LibraryID = lib.String
	r.Error = er.String
	return r, true, nil
}

func (s *Store) UpsertReplica(r domain.Replica) error {
	var aid any
	if r.AssetID != "" {
		aid = r.AssetID
	}
	_, err := s.DB.Exec(`INSERT INTO asset_replicas(logical_asset_id,member_id,immich_asset_id,role,filesystem_path,external_library_id,state,error) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(logical_asset_id,member_id) DO UPDATE SET immich_asset_id=excluded.immich_asset_id,role=excluded.role,filesystem_path=excluded.filesystem_path,external_library_id=excluded.external_library_id,state=excluded.state,error=excluded.error`, r.LogicalID, r.MemberID, aid, r.Role, r.Path, r.LibraryID, r.State, r.Error)
	return err
}

func (s *Store) UpsertReplicaFile(logicalID, memberID string, component domain.MediaComponent) error {
	_, err := s.DB.Exec(`INSERT INTO asset_replica_files(logical_asset_id,member_id,component_kind,source_path,recipient_path,state,source_size,source_mtime_ns) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(logical_asset_id,member_id,component_kind) DO UPDATE SET source_path=excluded.source_path,recipient_path=excluded.recipient_path,state=excluded.state,source_size=excluded.source_size,source_mtime_ns=excluded.source_mtime_ns`, logicalID, memberID, component.Kind, component.SourcePath, component.RecipientPath, component.State, component.SourceSize, component.SourceMtimeNS)
	return err
}

func (s *Store) ReplicaFiles(logicalID, memberID string) (map[string]domain.MediaComponent, error) {
	rows, err := s.DB.Query(`SELECT component_kind,source_path,recipient_path,state,source_size,source_mtime_ns FROM asset_replica_files WHERE logical_asset_id=? AND member_id=?`, logicalID, memberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]domain.MediaComponent{}
	for rows.Next() {
		var component domain.MediaComponent
		if err := rows.Scan(&component.Kind, &component.SourcePath, &component.RecipientPath, &component.State, &component.SourceSize, &component.SourceMtimeNS); err != nil {
			return nil, err
		}
		out[component.Kind] = component
	}
	return out, rows.Err()
}

func (s *Store) Replicas() ([]domain.Replica, error) {
	r, err := s.DB.Query(`SELECT logical_asset_id,member_id,COALESCE(immich_asset_id,''),role,COALESCE(filesystem_path,''),COALESCE(external_library_id,''),state,COALESCE(error,'') FROM asset_replicas ORDER BY logical_asset_id,member_id`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := []domain.Replica{}
	for r.Next() {
		var x domain.Replica
		if err := r.Scan(&x.LogicalID, &x.MemberID, &x.AssetID, &x.Role, &x.Path, &x.LibraryID, &x.State, &x.Error); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, r.Err()
}

func (s *Store) ReplicasFor(logical string) ([]domain.Replica, error) {
	all, err := s.Replicas()
	if err != nil {
		return nil, err
	}
	out := []domain.Replica{}
	for _, r := range all {
		if r.LogicalID == logical {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Store) Memberships(album string) (map[string]bool, error) {
	return queryIDs(s.DB, `SELECT logical_asset_id FROM album_memberships WHERE logical_album_id=?`, album)
}
func (s *Store) Observations(album, member string) (map[string]bool, error) {
	return queryIDs(s.DB, `SELECT immich_asset_id FROM album_observations WHERE logical_album_id=? AND member_id=?`, album, member)
}
func queryIDs(db *sql.DB, q string, args ...any) (map[string]bool, error) {
	r, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]bool{}
	for r.Next() {
		var x string
		if err := r.Scan(&x); err != nil {
			return nil, err
		}
		out[x] = true
	}
	return out, r.Err()
}

func (s *Store) SetMembership(album, logical string, present bool) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if present {
		_, err = tx.Exec(`INSERT OR IGNORE INTO album_memberships(logical_album_id,logical_asset_id) VALUES(?,?)`, album, logical)
		if err == nil {
			_, err = tx.Exec(`INSERT OR IGNORE INTO sharing_sources(logical_asset_id,source_kind,source_id) VALUES(?,'album',?)`, logical, album)
		}
	} else {
		_, err = tx.Exec(`DELETE FROM album_memberships WHERE logical_album_id=? AND logical_asset_id=?`, album, logical)
		if err == nil {
			_, err = tx.Exec(`DELETE FROM sharing_sources WHERE logical_asset_id=? AND source_kind='album' AND source_id=?`, logical, album)
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetObservations(album, member string, assets map[string]bool) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM album_observations WHERE logical_album_id=? AND member_id=?`, album, member); err != nil {
		return err
	}
	for id := range assets {
		if _, err = tx.Exec(`INSERT INTO album_observations(logical_album_id,member_id,immich_asset_id) VALUES(?,?,?)`, album, member, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) SetInitialized(album string) error {
	_, err := s.DB.Exec(`UPDATE logical_albums SET initialized=1 WHERE id=?`, album)
	return err
}
func (s *Store) SourceCount(logical string) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM sharing_sources WHERE logical_asset_id=?`, logical).Scan(&n)
	return n, err
}

func (s *Store) MarkUnused() error {
	_, err := s.DB.Exec(`UPDATE asset_replicas SET state='pending_removal',error=NULL WHERE role='external_replica' AND state NOT IN ('pending_removal','deleting') AND logical_asset_id NOT IN (SELECT logical_asset_id FROM sharing_sources)`)
	return err
}

func (s *Store) RemovalReplicas(limit int) ([]domain.Replica, error) {
	rows, err := s.DB.Query(`SELECT logical_asset_id,member_id,COALESCE(immich_asset_id,''),role,COALESCE(filesystem_path,''),COALESCE(external_library_id,''),state,COALESCE(error,'') FROM asset_replicas WHERE role='external_replica' AND state IN ('pending_removal','deleting') ORDER BY logical_asset_id,member_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Replica{}
	for rows.Next() {
		var replica domain.Replica
		if err := rows.Scan(&replica.LogicalID, &replica.MemberID, &replica.AssetID, &replica.Role, &replica.Path, &replica.LibraryID, &replica.State, &replica.Error); err != nil {
			return nil, err
		}
		out = append(out, replica)
	}
	return out, rows.Err()
}

func (s *Store) DeleteReplica(logicalID, memberID string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM asset_replica_files WHERE logical_asset_id=? AND member_id=?`, logicalID, memberID); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM asset_replicas WHERE logical_asset_id=? AND member_id=? AND role='external_replica' AND state IN ('pending_removal','deleting')`, logicalID, memberID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("replica removal state changed concurrently")
	}
	return tx.Commit()
}
