package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/0x464e/immich-family-bridge/internal/domain"
)

func (r *Reconciler) sharedWithTogether(album domain.Album) bool {
	for _, user := range album.Users {
		if strings.EqualFold(user.UserID, r.C.TogetherUserID) {
			return true
		}
	}
	return false
}

func (r *Reconciler) ensureTogetherShare(ctx context.Context, logicalID string, member domain.Member, album domain.Album) error {
	if r.C.TogetherUserID == "" || r.C.DryRun {
		return nil
	}
	if album.OwnerID != member.UserID {
		return fmt.Errorf("cannot share album %s: member %s is not its owner", album.ID, member.ID)
	}
	if !r.sharedWithTogether(album) {
		logical, err := r.DB.Album(logicalID)
		if err != nil {
			return err
		}
		confirmed, err := r.DB.ObservedAlbumMarker(logicalID, member.ID, strings.ToLower(r.C.TogetherUserID))
		if err != nil {
			return err
		}
		if confirmed && logical.SystemKey == "" {
			return errors.New("Together share was removed; album will be unmirrored on discovery")
		}
		if err := r.API.AddAlbumUser(ctx, member, album.ID, r.C.TogetherUserID); err != nil {
			return fmt.Errorf("share album %s with Together user: %w", album.ID, err)
		}
		r.Log.Info("mirror album shared with Together user", "member_id", member.ID, "immich_album_id", album.ID, "together_user_id", r.C.TogetherUserID)
	}
	return r.DB.RecordAlbumMarker(logicalID, member.ID, strings.ToLower(r.C.TogetherUserID))
}

// Discovery examines owned album metadata only. Registered replica IDs are
// skipped even if several members mark their copies, so names never establish
// identity. Archived mappings also block rediscovery during partial deletion.
func (r *Reconciler) discoverTogetherAlbums(ctx context.Context, preview bool) ([]Action, error) {
	if r.C.TogetherUserID == "" {
		return nil, nil
	}
	var actions []Action
	for _, member := range r.C.Members {
		albums, err := r.API.ListAlbums(ctx, member)
		if err != nil {
			return nil, fmt.Errorf("discover Together-shared albums for %s: %w", member.ID, err)
		}
		for _, album := range albums {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if album.OwnerID != member.UserID || !r.sharedWithTogether(album) {
				continue
			}
			if _, found, err := r.DB.AlbumByReplica(member.ID, album.ID); err != nil {
				return nil, err
			} else if found {
				continue
			}
			id, err := r.registerWithReplicas(ctx, member.ID, album.ID, nil, preview)
			if err != nil {
				return nil, fmt.Errorf("register Together-shared album %s: %w", album.ID, err)
			}
			actions = append(actions, Action{Kind: "register_mirror_album", AlbumID: id, AlbumName: album.Name, MemberID: member.ID})
			r.Log.Info("Together-shared album registered", "logical_album_id", id, "immich_album_id", album.ID, "member_id", member.ID, "dry_run", r.C.DryRun || preview)
		}
	}
	return actions, nil
}
