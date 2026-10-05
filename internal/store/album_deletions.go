package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/0x464e/immich-family-bridge/internal/domain"
)

func (s *Store) Album(id string) (domain.LogicalAlbum, error) {
	var a domain.LogicalAlbum
	err := s.DB.QueryRow(`SELECT id,name,description,COALESCE(cover_logical_asset_id,''),initialized,COALESCE(system_key,'') FROM logical_albums WHERE id=?`, id).
		Scan(&a.ID, &a.Name, &a.Description, &a.CoverID, &a.Initialized, &a.SystemKey)
	return a, err
}

func (s *Store) AlbumDeletions() ([]domain.AlbumDeletion, error) {
	rows, err := s.DB.Query(`SELECT record FROM album_deletions ORDER BY logical_album_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AlbumDeletion{}
	for rows.Next() {
		var raw string
		var record domain.AlbumDeletion
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (s *Store) AlbumDeletion(id string) (domain.AlbumDeletion, bool, error) {
	var raw string
	var record domain.AlbumDeletion
	err := s.DB.QueryRow(`SELECT record FROM album_deletions WHERE logical_album_id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, err
	}
	err = json.Unmarshal([]byte(raw), &record)
	return record, err == nil, err
}

// BeginAlbumDeletion atomically archives the album and stops both discovery
// and work from managing it, before the first remote DELETE. Together's photo
// sharing is retained even for an old secondary-only membership.
func (s *Store) BeginAlbumDeletion(record domain.AlbumDeletion) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var key string
	if err := tx.QueryRow(`SELECT COALESCE(system_key,'') FROM logical_albums WHERE id=?`, record.Album.ID).Scan(&key); err != nil {
		return err
	}
	if key != "" {
		return errors.New("system albums cannot be unmirrored")
	}
	if _, err := tx.Exec(`INSERT INTO album_deletions(logical_album_id,record) VALUES(?,?)`, record.Album.ID, string(raw)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO album_memberships(logical_album_id,logical_asset_id) SELECT t.id,m.logical_asset_id FROM album_memberships m JOIN logical_albums t ON t.system_key='together' WHERE m.logical_album_id=?`, record.Album.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO sharing_sources(logical_asset_id,source_kind,source_id) SELECT m.logical_asset_id,'album',t.id FROM album_memberships m JOIN logical_albums t ON t.system_key='together' WHERE m.logical_album_id=?`, record.Album.ID); err != nil {
		return err
	}
	for _, table := range []string{"album_memberships", "album_observations", "album_cover_observations", "public_link_mappings", "album_marker_observations"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE logical_album_id=?`, record.Album.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM sharing_sources WHERE source_kind='album' AND source_id=?`, record.Album.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveAlbumDeletion(record domain.AlbumDeletion) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := s.DB.Exec(`UPDATE album_deletions SET record=? WHERE logical_album_id=?`, string(raw), record.Album.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return sql.ErrNoRows
	}
	return err
}

func (s *Store) FinishAlbumRestore(id string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE logical_albums SET initialized=0,cover_logical_asset_id=NULL WHERE id=? AND system_key IS NULL`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM album_deletions WHERE logical_album_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordAlbumMarker(album, member, marker string) error {
	_, err := s.DB.Exec(`INSERT INTO album_marker_observations(logical_album_id,member_id,marker_user_id) VALUES(?,?,?) ON CONFLICT(logical_album_id,member_id) DO UPDATE SET marker_user_id=excluded.marker_user_id`, album, member, marker)
	return err
}

func (s *Store) ObservedAlbumMarker(album, member, marker string) (bool, error) {
	var found int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM album_marker_observations WHERE logical_album_id=? AND member_id=? AND marker_user_id=?`, album, member, marker).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("album marker observation: %w", err)
	}
	return found != 0, nil
}
