package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
	"github.com/0x464e/immich-family-bridge/internal/store"
)

var ErrProtectedAlbum = errors.New("the catch-all Together/system album cannot be unmirrored")

func (r *Reconciler) UnmirrorAlbum(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.C.DryRun {
		return ErrDryRunMode
	}
	return r.unmirrorAlbum(ctx, id, "api")
}

func (r *Reconciler) unmirrorAlbum(ctx context.Context, id, requestedBy string) error {
	album, err := r.DB.Album(id)
	if err != nil {
		return err
	}
	if album.SystemKey != "" {
		return ErrProtectedAlbum
	}
	record, found, err := r.DB.AlbumDeletion(id)
	if err != nil {
		return err
	}
	if found {
		if record.State == "restoring" {
			return errors.New("album restoration is in progress")
		}
		return r.finishAlbumDeletion(ctx, &record)
	}
	replicas, err := r.DB.AlbumReplicas(id)
	if err != nil {
		return err
	}
	record = domain.AlbumDeletion{Album: album, DeletedAt: r.now().UTC().Format(time.RFC3339Nano), RequestedBy: requestedBy, State: "deleting"}
	// Read every replica successfully and validate ownership before saving intent
	// or deleting anything. A timeout/403/failed search is never an empty album.
	for _, member := range r.C.Members {
		rep := domain.DeletedAlbumReplica{MemberID: member.ID, AlbumID: replicas[member.ID], AssetIDs: []string{}}
		if rep.AlbumID != "" {
			remote, err := r.API.GetAlbum(ctx, member, rep.AlbumID)
			if err != nil {
				return err
			}
			if remote.OwnerID != member.UserID {
				return errors.New("album deletion owner mismatch")
			}
			assets, err := r.API.ListAlbumAssets(ctx, member, rep.AlbumID)
			if err != nil {
				return err
			}
			for _, asset := range assets {
				rep.AssetIDs = append(rep.AssetIDs, asset.ID)
			}
			rep.CoverID = remote.CoverID
		}
		record.Replicas = append(record.Replicas, rep)
	}
	if err := r.DB.BeginAlbumDeletion(record); err != nil {
		return err
	}
	r.Log.Info("mirror album deletion archived", "logical_album_id", id, "album_name", album.Name, "requested_by", requestedBy)
	return r.finishAlbumDeletion(ctx, &record)
}

// Defense in depth: neither stale/corrupt recovery data nor a retry can target
// a catch-all replica. System identity, not a user-editable album name, protects it.
func (r *Reconciler) protectedAlbumID(id string) (bool, error) {
	var count int
	err := r.DB.DB.QueryRow(`SELECT COUNT(*) FROM album_replicas ar JOIN logical_albums a ON a.id=ar.logical_album_id WHERE a.system_key IS NOT NULL AND ar.immich_album_id=?`, id).Scan(&count)
	return count != 0, err
}

func (r *Reconciler) finishAlbumDeletion(ctx context.Context, record *domain.AlbumDeletion) error {
	album, err := r.DB.Album(record.Album.ID)
	if err != nil {
		return err
	}
	if album.SystemKey != "" || record.Album.SystemKey != "" {
		return ErrProtectedAlbum
	}
	if record.State == "deleted" {
		return nil
	}
	if record.State != "deleting" {
		return errors.New("album is not being deleted")
	}
	for i := range record.Replicas {
		rep := &record.Replicas[i]
		if rep.Deleted {
			continue
		}
		member, found := r.member(rep.MemberID)
		if !found {
			return errors.New("deleted album member is no longer configured")
		}
		if rep.AlbumID != "" {
			protected, err := r.protectedAlbumID(rep.AlbumID)
			if err != nil {
				return err
			}
			if protected {
				return ErrProtectedAlbum
			}
			remote, err := r.API.GetAlbum(ctx, member, rep.AlbumID)
			if err != nil && !errors.Is(err, immich.ErrNotFound) {
				return err
			}
			if err == nil {
				if remote.OwnerID != member.UserID {
					return errors.New("album deletion owner mismatch")
				}
				if err := r.API.DeleteAlbum(ctx, member, rep.AlbumID); err != nil && !errors.Is(err, immich.ErrNotFound) {
					return err
				}
			}
		}
		rep.Deleted = true
		if err := r.DB.SaveAlbumDeletion(*record); err != nil {
			return err
		}
	}
	record.State = "deleted"
	if err := r.DB.SaveAlbumDeletion(*record); err != nil {
		return err
	}
	r.Log.Info("mirror album deleted; photos retained", "logical_album_id", record.Album.ID, "album_name", record.Album.Name)
	return nil
}

func (r *Reconciler) RestoreAlbum(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.C.DryRun {
		return ErrDryRunMode
	}
	album, err := r.DB.Album(id)
	if err != nil {
		return err
	}
	if album.SystemKey != "" {
		return ErrProtectedAlbum
	}
	record, found, err := r.DB.AlbumDeletion(id)
	if err != nil || !found {
		return err
	} // Already restored: idempotent success.
	if record.State == "deleting" {
		return errors.New("album deletion must finish before restoration")
	}
	if record.State == "deleted" {
		record.State, record.RestoreToken = "restoring", store.ID()
		if err := r.DB.SaveAlbumDeletion(record); err != nil {
			return err
		}
	}
	return r.finishAlbumRestore(ctx, &record)
}

func (r *Reconciler) finishAlbumRestore(ctx context.Context, record *domain.AlbumDeletion) error {
	album, err := r.DB.Album(record.Album.ID)
	if err != nil {
		return err
	}
	if album.SystemKey != "" || record.Album.SystemKey != "" {
		return ErrProtectedAlbum
	}
	if record.State != "restoring" || record.RestoreToken == "" {
		return errors.New("invalid album restoration state")
	}
	for i := range record.Replicas {
		rep := &record.Replicas[i]
		if rep.Restored {
			continue
		}
		member, found := r.member(rep.MemberID)
		if !found {
			return errors.New("deleted album member is no longer configured")
		}
		if rep.RestoredAlbumID == "" {
			// A unique temporary description lets retries adopt a successful create
			// whose response/progress commit was lost, without guessing by name.
			tag := "[familybridge restore " + record.RestoreToken + "/" + member.ID + "]"
			albums, err := r.API.ListAlbums(ctx, member)
			if err != nil {
				return err
			}
			for _, remote := range albums {
				if remote.OwnerID == member.UserID && remote.Description == tag {
					if rep.RestoredAlbumID != "" {
						return errors.New("multiple albums carry the restoration token")
					}
					rep.RestoredAlbumID = remote.ID
				}
			}
			if rep.RestoredAlbumID == "" {
				remote, err := r.API.CreateAlbum(ctx, member, record.RestoreToken, record.Album.Name, tag)
				if err != nil {
					return err
				}
				if remote.OwnerID != member.UserID {
					return errors.New("restored album owner mismatch")
				}
				rep.RestoredAlbumID = remote.ID
			}
			if err := r.DB.SaveAlbumDeletion(*record); err != nil {
				return err
			}
		}
		remote, err := r.API.GetAlbum(ctx, member, rep.RestoredAlbumID)
		if err != nil {
			return err
		}
		if remote.OwnerID != member.UserID {
			return errors.New("restored album owner mismatch")
		}
		protected, err := r.protectedAlbumID(remote.ID)
		if err != nil {
			return err
		}
		if protected {
			return ErrProtectedAlbum
		}
		ids := []string{}
		cover := ""
		for _, id := range rep.AssetIDs {
			asset, err := r.API.GetAsset(ctx, member, id)
			if errors.Is(err, immich.ErrNotFound) {
				r.Log.Warn("album restoration skipped missing asset", "logical_album_id", record.Album.ID, "member_id", member.ID, "immich_asset_id", id)
				continue
			}
			if err != nil {
				return err
			}
			if asset.OwnerID != member.UserID {
				r.Log.Warn("album restoration skipped foreign asset", "logical_album_id", record.Album.ID, "member_id", member.ID, "immich_asset_id", id)
				continue
			}
			ids = append(ids, id)
			if id == rep.CoverID {
				cover = id
			}
		}
		for start := 0; start < len(ids); start += albumBatchSize {
			end := min(start+albumBatchSize, len(ids))
			if err := r.API.AddAssets(ctx, member, remote.ID, ids[start:end]); err != nil {
				return err
			}
		}
		// Immich can return HTTP 200 with individual asset failures. Do not discard
		// the recovery record until membership is actually present. An asset that
		// vanished between validation and the batch write is still safely skipped.
		actual, err := r.API.ListAlbumAssets(ctx, member, remote.ID)
		if err != nil {
			return err
		}
		present := map[string]bool{}
		for _, asset := range actual {
			present[asset.ID] = true
		}
		for _, id := range ids {
			if present[id] {
				continue
			}
			if _, err := r.API.GetAsset(ctx, member, id); errors.Is(err, immich.ErrNotFound) {
				r.Log.Warn("album restoration skipped missing asset", "logical_album_id", record.Album.ID, "member_id", member.ID, "immich_asset_id", id)
				if cover == id {
					cover = ""
				}
			} else if err != nil {
				return err
			} else {
				return fmt.Errorf("restored album %s is missing asset %s after addition", remote.ID, id)
			}
		}
		if err := r.API.UpdateAlbum(ctx, member, remote.ID, record.Album.Name, record.Album.Description, cover); err != nil {
			return err
		}
		if err := r.DB.SetAlbumReplica(record.Album.ID, member.ID, remote.ID); err != nil {
			return err
		}
		if err := r.ensureTogetherShare(ctx, record.Album.ID, member, remote); err != nil {
			return err
		}
		rep.Restored = true
		if err := r.DB.SaveAlbumDeletion(*record); err != nil {
			return err
		}
	}
	if err := r.DB.FinishAlbumRestore(record.Album.ID); err != nil {
		return err
	}
	r.Log.Info("mirror album restored", "logical_album_id", record.Album.ID, "album_name", record.Album.Name)
	return nil
}

func (r *Reconciler) resumeAlbumOperations(ctx context.Context) error {
	records, err := r.DB.AlbumDeletions()
	if err != nil {
		return err
	}
	for i := range records {
		switch records[i].State {
		case "deleting":
			err = r.finishAlbumDeletion(ctx, &records[i])
		case "restoring":
			err = r.finishAlbumRestore(ctx, &records[i])
		case "deleted":
			continue
		default:
			return errors.New("invalid archived album state")
		}
		if err != nil {
			return fmt.Errorf("resume album %s: %w", records[i].Album.ID, err)
		}
	}
	return nil
}

// Only a share previously confirmed for this exact marker user can signal
// unmirroring. Legacy registrations and a newly configured marker are backfilled.
func (r *Reconciler) missingAlbumMarkers(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if r.C.TogetherUserID == "" {
		return out, nil
	}
	albums, err := r.DB.Albums()
	if err != nil {
		return nil, err
	}
	for _, album := range albums {
		if album.SystemKey != "" {
			continue
		}
		reps, err := r.DB.AlbumReplicas(album.ID)
		if err != nil {
			return nil, err
		}
		for _, member := range r.C.Members {
			confirmed, err := r.DB.ObservedAlbumMarker(album.ID, member.ID, strings.ToLower(r.C.TogetherUserID))
			if err != nil {
				return nil, err
			}
			if !confirmed || reps[member.ID] == "" {
				continue
			}
			remote, err := r.API.GetAlbum(ctx, member, reps[member.ID])
			if err != nil {
				return nil, err
			}
			if remote.OwnerID != member.UserID {
				return nil, errors.New("album marker owner mismatch")
			}
			if !r.sharedWithTogether(remote) {
				out[album.ID] = "member:" + member.ID
			}
		}
	}
	return out, nil
}
