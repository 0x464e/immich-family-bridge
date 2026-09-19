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
		if remote.Name != a.Name || remote.Description != a.Description {
			p.actions = append(p.actions, Action{Kind: "update_album_metadata", MemberID: m.ID})
		}
		o.coverID = remote.CoverID
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
		for _, asset := range assets {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			current[asset.ID] = true
			lid, known, err := r.DB.FindReplicaAsset(m.ID, asset.ID)
			if err != nil {
				return nil, err
			}
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
				_, err := r.API.GetAsset(ctx, m, old)
				if errors.Is(err, immich.ErrNotFound) {
					if preview {
						p.actions = append(p.actions, Action{Kind: "source_missing", MemberID: m.ID, ImmichAssetID: old})
					} else {
						r.Log.Warn("asset missing from Immich; keeping sharing reference", "member_id", m.ID, "immich_asset_id", old)
					}
					continue
				}
				if err != nil {
					return nil, fmt.Errorf("check missing album asset: %w", err)
				}
				lid, known, err := r.DB.FindReplicaAsset(m.ID, old)
				if err != nil {
					return nil, err
				}
				if known {
					p.removes[lid] = true
				}
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
			if _, err = tx.Exec("DELETE FROM album_observations WHERE logical_album_id=? AND member_id=?", p.album.ID, o.member.ID); err != nil {
				return err
			}
			for id := range o.assets {
				if _, err = tx.Exec("INSERT INTO album_observations(logical_album_id,member_id,immich_asset_id) VALUES(?,?,?)", p.album.ID, o.member.ID, id); err != nil {
					return err
				}
			}
		}
		if _, err = tx.Exec("UPDATE logical_albums SET initialized=1 WHERE id=?", p.album.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	actions := []Action{}
	desiredAnywhere := map[string]bool{}
	for _, p := range plans {
		add := func(a Action) { a.AlbumID = p.album.ID; a.AlbumName = p.album.Name; actions = append(actions, a) }
		for _, a := range p.actions {
			add(a)
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
	if r.C.RemoveUnshared {
		replicas, err := r.DB.Replicas()
		if err != nil {
			return nil, err
		}
		for _, rep := range replicas {
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
