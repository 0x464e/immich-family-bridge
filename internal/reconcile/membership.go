package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
)

type albumPlan struct {
	album                          domain.LogicalAlbum
	observations                   []observed
	before, desired, adds, removes map[string]bool
	actions                        []Action
	origins                        map[string]domain.Asset
	originMembers                  map[string]string
	assetLogical                   map[string]string
	ownerDeletes                   map[string]bool
	ownerRestores                  map[string]bool
	repairs                        map[string]domain.Replica
}

func (r *Reconciler) observeAlbum(ctx context.Context, a domain.LogicalAlbum, preview bool) (*albumPlan, error) {
	before, err := r.DB.Memberships(a.ID)
	if err != nil {
		return nil, err
	}
	p := &albumPlan{album: a, before: before, desired: map[string]bool{}, adds: map[string]bool{}, removes: map[string]bool{}, origins: map[string]domain.Asset{}, originMembers: map[string]string{}, assetLogical: map[string]string{}}
	for id := range before {
		p.desired[id] = true
	}
	reps, err := r.DB.AlbumReplicas(a.ID)
	if err != nil {
		return nil, err
	}
	for _, m := range r.C.Members {
		albumID := reps[m.ID]
		current := map[string]bool{}
		o := observed{member: m, albumID: albumID, assets: current}
		if albumID == "" {
			if !preview {
				return nil, fmt.Errorf("missing album replica for %s", m.ID)
			}
			p.actions = append(p.actions, Action{Kind: "create_album_replica", MemberID: m.ID})
			if r.C.TogetherUserID != "" {
				p.actions = append(p.actions, Action{Kind: "share_album", MemberID: m.ID})
			}
			p.observations = append(p.observations, o)
			continue
		}
		remote, err := r.API.GetAlbum(ctx, m, albumID)
		if err != nil {
			return nil, err
		}
		if remote.OwnerID != "" && remote.OwnerID != m.UserID {
			return nil, fmt.Errorf("album owner mismatch for %s", m.ID)
		}
		if preview && r.C.TogetherUserID != "" && !r.sharedWithTogether(remote) {
			p.actions = append(p.actions, Action{Kind: "share_album", MemberID: m.ID})
		}
		if remote.Name != a.Name || remote.Description != a.Description {
			p.actions = append(p.actions, Action{Kind: "update_album_metadata", MemberID: m.ID})
		}
		o.coverID = remote.CoverID
		o.name, o.description = remote.Name, remote.Description
		o.previousCoverID, o.coverWasObserved, err = r.DB.AlbumCoverObservation(a.ID, m.ID)
		if err != nil {
			return nil, err
		}
		assets, err := r.API.ListAlbumAssets(ctx, m, albumID)
		if err != nil {
			return nil, err
		}
		prior, err := r.DB.Observations(a.ID, m.ID)
		if err != nil {
			return nil, err
		}
		o.prior = prior
		mappedAssets := r.observedAssetIDs[m.ID]
		if mappedAssets == nil {
			mappedAssets, err = r.DB.ReplicaAssetIDs(m.ID)
			if err != nil {
				return nil, err
			}
		}
		for _, asset := range assets {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if asset.IsTrashed {
				continue
			}
			current[asset.ID] = true
			lid, known := mappedAssets[asset.ID]
			if !known {
				if asset.OwnerID != m.UserID || asset.LibraryID == m.LibraryID {
					if !preview {
						return nil, fmt.Errorf("unknown bridge or foreign asset %s in %s album", asset.ID, m.ID)
					}
					p.actions = append(p.actions, Action{Kind: "mapping_inconsistency", MemberID: m.ID, ImmichAssetID: asset.ID})
					continue
				}
				if preview {
					asset, err = r.API.GetAsset(ctx, m, asset.ID)
					if err != nil {
						return nil, err
					}
					if asset.OwnerID != m.UserID || asset.LibraryID == m.LibraryID {
						return nil, errors.New("source identity changed")
					}
					if !asset.Supported() {
						p.actions = append(p.actions, Action{Kind: "unsupported_asset", MemberID: m.ID, ImmichAssetID: asset.ID})
						continue
					}
				}
				mapped, err := r.mappedAsset(asset)
				if err == nil && preview {
					_, err = r.FS.SourceInfo(mapped.OriginalPath)
				}
				if err != nil {
					if !preview {
						return nil, err
					}
					p.actions = append(p.actions, Action{Kind: "source_error", MemberID: m.ID, ImmichAssetID: asset.ID, Error: err.Error()})
					continue
				}
				if preview {
					lid = "new:" + m.ID + ":" + asset.ID
					p.origins[lid] = asset
					p.originMembers[lid] = m.ID
					p.actions = append(p.actions, Action{Kind: "discover_origin", MemberID: m.ID, ImmichAssetID: asset.ID})
				} else {
					lid, err = r.DB.EnsureOrigin(r.C.FamilyID, m.ID, mapped)
					if err != nil {
						return nil, err
					}
					mappedAssets[asset.ID] = lid
				}
			}
			if !preview && asset.OwnerID == m.UserID && asset.LibraryID != m.LibraryID && asset.StackID != "" {
				if err := r.rememberSourceStack(m.ID, asset); err != nil {
					return nil, err
				}
			}
			p.assetLogical[m.ID+"\x00"+asset.ID] = lid
			if !a.Initialized || !prior[asset.ID] {
				p.adds[lid] = true
			}
		}
		if a.Initialized {
			for old := range prior {
				if current[old] {
					continue
				}
				lid, known := mappedAssets[old]
				if !known {
					continue
				}
				asset, err := r.API.GetAsset(ctx, m, old)
				if err != nil && !errors.Is(err, immich.ErrNotFound) {
					return nil, fmt.Errorf("check missing album asset: %w", err)
				}
				// Deleting a recipient copy is never an unshare instruction.
				// Audits restore trash or reimport a permanently missing copy.
				if errors.Is(err, immich.ErrNotFound) || asset.IsTrashed {
					rep, found, e := r.DB.Replica(lid, m.ID)
					if e != nil {
						return nil, e
					}
					if found && rep.Role == "external_replica" {
						if p.repairs == nil {
							p.repairs = map[string]domain.Replica{}
						}
						p.repairs[m.ID+"\x00"+lid] = rep
					}
					continue
				}
				p.removes[lid] = true
			}
		}
		p.observations = append(p.observations, o)
	}
	for id := range p.adds {
		p.desired[id] = true
	}
	for id := range p.removes {
		if !p.adds[id] {
			delete(p.desired, id)
		}
	}
	if err := r.resolveAlbumCover(p); err != nil {
		return nil, err
	}
	return p, nil
}

// resolveAlbumCover adopts a member's changed cover when it refers to a known
// logical asset in the mirrored album. Observations are recorded only after a
// successful bridge write, so a partial propagation retry is not mistaken for
// another member editing their cover.
func (r *Reconciler) resolveAlbumCover(p *albumPlan) error {
	coverLogicalID := ""
	coverMemberID := ""
	for _, o := range p.observations {
		if !o.coverWasObserved || o.coverID == "" || o.coverID == o.previousCoverID {
			continue
		}
		logicalID, known := p.assetLogical[o.member.ID+"\x00"+o.coverID]
		if !known || !p.desired[logicalID] {
			continue
		}
		if coverLogicalID == "" {
			coverLogicalID, coverMemberID = logicalID, o.member.ID
			continue
		}
		if coverLogicalID != logicalID {
			p.actions = append(p.actions, Action{Kind: "album_cover_conflict", MemberID: o.member.ID, LogicalAssetID: logicalID})
		}
	}
	if coverLogicalID != "" && coverLogicalID != p.album.CoverID {
		p.album.CoverID = coverLogicalID
		p.actions = append(p.actions, Action{Kind: "adopt_album_cover", MemberID: coverMemberID, LogicalAssetID: coverLogicalID})
	}
	if p.album.CoverID == "" {
		return nil
	}
	for _, o := range p.observations {
		cover, found, err := r.DB.Replica(p.album.CoverID, o.member.ID)
		if err != nil {
			return err
		}
		if found && cover.State == "ready" && cover.AssetID != "" && o.coverID != cover.AssetID {
			p.actions = append(p.actions, Action{Kind: "update_album_cover", MemberID: o.member.ID, LogicalAssetID: p.album.CoverID, ImmichAssetID: cover.AssetID})
		}
	}
	return nil
}

// Together owns the shared set. Secondary album membership is a subset;
// removing secondary membership never retracts the Together membership.
// A direct Together addition wins a concurrent Together removal. Otherwise
// an explicit Together removal overrides secondary additions in that cycle.
func resolveTogether(plans []*albumPlan) {
	var together *albumPlan
	for _, p := range plans {
		if p.album.SystemKey == "together" {
			together = p
			break
		}
	}
	if together == nil {
		return
	}
	for _, p := range plans {
		if p == together {
			continue
		}
		for id := range p.desired {
			if together.removes[id] && !together.adds[id] {
				delete(p.desired, id)
				continue
			}
			together.desired[id] = true
		}
	}
	// Together must be written before secondary replicas get their assets.
	sort.SliceStable(plans, func(i, j int) bool { return plans[i] == together && plans[j] != together })
}

// Persist the entire family decision and its observed inputs in one SQLite
// transaction. After a crash, unfinished remote writes cannot resurrect a
// removed asset merely because another album still has its old contents.
func (r *Reconciler) persistPlans(plans []*albumPlan) error {
	tx, err := r.DB.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range plans {
		for _, rep := range p.repairs {
			if _, err := tx.Exec("UPDATE asset_replicas SET state='repair_pending' WHERE logical_asset_id=? AND member_id=? AND role='external_replica'", rep.LogicalID, rep.MemberID); err != nil {
				return err
			}
		}
		for id := range p.ownerRestores {
			if _, err := tx.Exec("UPDATE asset_replicas SET state='ready',error=NULL WHERE logical_asset_id=? AND role='origin'", id); err != nil {
				return err
			}
		}
		for id := range p.ownerDeletes {
			if _, err := tx.Exec("UPDATE asset_replicas SET state='owner_deleted',error=NULL WHERE logical_asset_id=? AND role='origin'", id); err != nil {
				return err
			}
		}
		var cover any
		if p.album.CoverID != "" {
			cover = p.album.CoverID
		}
		if _, err := tx.Exec(`UPDATE logical_albums SET cover_logical_asset_id=? WHERE id=?`, cover, p.album.ID); err != nil {
			return err
		}
		for id := range p.before {
			if p.desired[id] {
				continue
			}
			if _, err = tx.Exec("DELETE FROM album_memberships WHERE logical_album_id=? AND logical_asset_id=?", p.album.ID, id); err != nil {
				return err
			}
			if _, err = tx.Exec("DELETE FROM sharing_sources WHERE source_kind='album' AND source_id=? AND logical_asset_id=?", p.album.ID, id); err != nil {
				return err
			}
		}
		for id := range p.desired {
			if p.before[id] {
				continue
			}
			if _, err = tx.Exec("INSERT OR IGNORE INTO album_memberships(logical_album_id,logical_asset_id) VALUES(?,?)", p.album.ID, id); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT OR IGNORE INTO sharing_sources(logical_asset_id,source_kind,source_id) VALUES(?,'album',?)", id, p.album.ID); err != nil {
				return err
			}
		}
		for _, o := range p.observations {
			if err := r.DB.UpdateObservationsTx(tx, p.album.ID, o.member.ID, o.prior, o.assets); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("UPDATE logical_albums SET initialized=1 WHERE id=?", p.album.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, p := range plans {
		for i := range p.observations {
			o := &p.observations[i]
			o.persisted = make(map[string]bool, len(o.assets))
			for id := range o.assets {
				o.persisted[id] = true
			}
		}
	}
	return nil
}

func (r *Reconciler) previewPlans(ctx context.Context, albums []domain.LogicalAlbum) ([]Action, error) {
	plans := []*albumPlan{}
	origins := map[string]domain.Asset{}
	members := map[string]string{}
	for _, a := range albums {
		p, err := r.observeAlbum(ctx, a, true)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
		for id, asset := range p.origins {
			origins[id] = asset
			members[id] = p.originMembers[id]
		}
	}
	resolveTogether(plans)
	if err := r.resolveOwnerDeletions(ctx, plans); err != nil {
		return nil, err
	}
	actions := []Action{}
	desiredAnywhere := map[string]bool{}
	for _, p := range plans {
		add := func(a Action) { a.AlbumID = p.album.ID; a.AlbumName = p.album.Name; actions = append(actions, a) }
		for _, a := range p.actions {
			add(a)
		}
		for _, rep := range p.repairs {
			if p.desired[rep.LogicalID] {
				add(Action{Kind: "restore_recipient_replica", MemberID: rep.MemberID, LogicalAssetID: rep.LogicalID, ImmichAssetID: rep.AssetID})
			}
		}
		for id := range p.desired {
			desiredAnywhere[id] = true
			if !p.before[id] {
				add(Action{Kind: "add_sharing_reference", LogicalAssetID: id})
			}
		}
		for id := range p.before {
			if !p.desired[id] {
				add(Action{Kind: "remove_sharing_reference", LogicalAssetID: id})
			}
		}
		for _, o := range p.observations {
			for id := range p.desired {
				rep, found, err := r.DB.Replica(id, o.member.ID)
				if err != nil {
					return nil, err
				}
				if source, ok := origins[id]; ok {
					if members[id] == o.member.ID {
						if !o.assets[source.ID] {
							add(Action{Kind: "add_album_asset", MemberID: o.member.ID, ImmichAssetID: source.ID, LogicalAssetID: id})
						}
					} else {
						add(Action{Kind: "link_and_import", MemberID: o.member.ID, SourceMemberID: members[id], ImmichAssetID: source.ID, LogicalAssetID: id})
					}
				} else if !found || rep.AssetID == "" {
					add(Action{Kind: "link_and_import", MemberID: o.member.ID, LogicalAssetID: id})
				} else if !o.assets[rep.AssetID] {
					add(Action{Kind: "add_album_asset", MemberID: o.member.ID, LogicalAssetID: id, ImmichAssetID: rep.AssetID})
				}
			}
			for assetID := range o.assets {
				lid, known, err := r.DB.FindReplicaAsset(o.member.ID, assetID)
				if err != nil {
					return nil, err
				}
				if known && !p.desired[lid] {
					add(Action{Kind: "remove_album_asset", MemberID: o.member.ID, LogicalAssetID: lid, ImmichAssetID: assetID})
				}
			}
		}
	}
	{
		replicas, err := r.DB.Replicas()
		if err != nil {
			return nil, err
		}
		deleted := map[string]bool{}
		for _, p := range plans {
			for id := range p.ownerDeletes {
				deleted[id] = true
			}
		}
		for _, rep := range replicas {
			if rep.Role == "origin" && rep.State == "owner_deleted" {
				deleted[rep.LogicalID] = true
			}
		}
		for _, rep := range replicas {
			if !r.C.RemoveUnshared && !deleted[rep.LogicalID] {
				continue
			}
			if rep.Role != "external_replica" || desiredAnywhere[rep.LogicalID] {
				continue
			}
			// Future non-album sharing sources must also protect a replica.
			var n int
			if err := r.DB.DB.QueryRow("SELECT COUNT(*) FROM sharing_sources WHERE logical_asset_id=? AND source_kind!='album'", rep.LogicalID).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				actions = append(actions, Action{Kind: "delete_recipient_replica", MemberID: rep.MemberID, LogicalAssetID: rep.LogicalID, ImmichAssetID: rep.AssetID})
			}
		}
	}
	sort.Slice(actions, func(i, j int) bool {
		a, b := actions[i], actions[j]
		left := []string{a.AlbumID, a.Kind, a.MemberID, a.SourceMemberID, a.LogicalAssetID, a.ImmichAssetID}
		right := []string{b.AlbumID, b.Kind, b.MemberID, b.SourceMemberID, b.LogicalAssetID, b.ImmichAssetID}
		for k := range left {
			if left[k] != right[k] {
				return left[k] < right[k]
			}
		}
		return false
	})
	return actions, nil
}

// Owner deletion overrides additions and removals in every mirrored album.
// Inspect origins absent from their own observed albums, including older
// source_missing records whose last observation has already been forgotten.
func (r *Reconciler) resolveOwnerDeletions(ctx context.Context, plans []*albumPlan) error {
	candidates, present := map[string]bool{}, map[string]bool{}
	for _, p := range plans {
		for id := range p.desired {
			candidates[id] = true
		}
		for id := range p.before {
			candidates[id] = true
		}
	}
	reps, err := r.DB.Replicas()
	if err != nil {
		return err
	}
	origins := map[string]domain.Replica{}
	for _, rep := range reps {
		if rep.Role == "origin" {
			origins[rep.LogicalID] = rep
		}
	}
	for _, p := range plans {
		for _, o := range p.observations {
			for aid := range o.assets {
				id := p.assetLogical[o.member.ID+"\x00"+aid]
				if rep, ok := origins[id]; ok && rep.MemberID == o.member.ID && rep.AssetID == aid {
					present[id] = true
				}
			}
		}
	}
	deleted, restored := map[string]bool{}, map[string]bool{}
	for id := range candidates {
		rep, known := origins[id]
		if !known {
			continue
		} // Newly discovered dry-run origin.
		if rep.State == "owner_deleted" {
			// Only an active origin appearing in its owner's album can reshare
			// a previously deleted asset. A stale recipient cannot do so.
			if present[id] {
				restored[id] = true
			} else {
				deleted[id] = true
			}
			continue
		}
		if present[id] {
			continue
		}
		member, ok := r.member(rep.MemberID)
		if !ok {
			return errors.New("origin member missing")
		}
		asset, err := r.API.GetAsset(ctx, member, rep.AssetID)
		if err != nil && !errors.Is(err, immich.ErrNotFound) {
			return fmt.Errorf("confirm owner deletion: %w", err)
		}
		if err == nil && asset.OwnerID != member.UserID {
			return errors.New("origin owner mismatch")
		}
		if errors.Is(err, immich.ErrNotFound) || asset.IsTrashed {
			deleted[id] = true
		}
	}
	for _, p := range plans {
		p.ownerDeletes = deleted
		p.ownerRestores = restored
		for id := range deleted {
			delete(p.desired, id)
			delete(p.adds, id)
			p.removes[id] = true
		}
	}
	return nil
}
