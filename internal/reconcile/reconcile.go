package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/0x464e/immich-family-bridge/internal/config"
	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/filesystem"
	"github.com/0x464e/immich-family-bridge/internal/immich"
	"github.com/0x464e/immich-family-bridge/internal/store"
)

type Reconciler struct {
	C   config.Config
	DB  *store.Store
	API immich.Client
	FS  filesystem.Linker
	Log *slog.Logger
	mu  sync.Mutex
}

func New(c config.Config, db *store.Store, api immich.Client, log *slog.Logger) *Reconciler {
	return &Reconciler{C: c, DB: db, API: api, FS: filesystem.Linker{SourceRoot: c.SourceRoot, BridgeRoot: c.BridgeRoot}, Log: log}
}
func (r *Reconciler) member(id string) (domain.Member, bool) {
	for _, m := range r.C.Members {
		if m.ID == id {
			return m, true
		}
	}
	return domain.Member{}, false
}

func (r *Reconciler) Register(ctx context.Context, memberID, albumID string) (string, error) {
	return r.RegisterWithReplicas(ctx, memberID, albumID, nil)
}

func (r *Reconciler) RegisterWithReplicas(ctx context.Context, memberID, albumID string, existing map[string]string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.member(memberID)
	if !ok {
		return "", errors.New("unknown member")
	}
	if id, found, err := r.DB.AlbumByReplica(memberID, albumID); err != nil || found {
		if err != nil {
			return id, err
		}
		albums, e := r.DB.Albums()
		if e != nil {
			return id, e
		}
		for _, a := range albums {
			if a.ID == id {
				return id, r.ensureAlbumReplicas(ctx, a)
			}
		}
		return id, errors.New("album mapping missing")
	}
	a, err := r.API.GetAlbum(ctx, m, albumID)
	if err != nil {
		return "", err
	}
	if a.OwnerID != m.UserID {
		return "", errors.New("album is not owned by member")
	}
	id := store.ID()
	logical := domain.LogicalAlbum{ID: id, Name: a.Name, Description: a.Description}
	replicas := map[string]string{memberID: albumID}
	for otherID, otherAlbumID := range existing {
		if otherID == memberID {
			if otherAlbumID != albumID {
				return "", errors.New("source album ID conflicts with replicas")
			}
			continue
		}
		other, ok := r.member(otherID)
		if !ok || otherAlbumID == "" {
			return "", errors.New("invalid existing album replica")
		}
		if _, found, e := r.DB.AlbumByReplica(otherID, otherAlbumID); e != nil {
			return "", e
		} else if found {
			return "", errors.New("existing album already registered")
		}
		remote, e := r.API.GetAlbum(ctx, other, otherAlbumID)
		if e != nil {
			return "", e
		}
		if remote.OwnerID != other.UserID {
			return "", errors.New("existing album owner mismatch")
		}
		replicas[otherID] = otherAlbumID
	}
	if err := r.DB.AddAlbum(r.C.FamilyID, logical, replicas); err != nil {
		return "", err
	}
	if a.CoverID != "" {
		asset, e := r.API.GetAsset(ctx, m, a.CoverID)
		if e == nil && asset.OwnerID == m.UserID {
			if lid, e := r.DB.EnsureOrigin(r.C.FamilyID, m.ID, asset); e == nil {
				_ = r.DB.SetCover(id, lid)
			}
		}
	}
	if err := r.ensureAlbumReplicas(ctx, logical); err != nil {
		return id, err
	}
	return id, nil
}

func (r *Reconciler) UpdateAlbum(id, name, description, cover string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		return errors.New("album name required")
	}
	if cover != "" {
		if _, err := r.DB.LogicalAsset(cover); err != nil {
			return errors.New("unknown logical cover asset")
		}
	}
	return r.DB.UpdateAlbum(id, name, description, cover)
}

func (r *Reconciler) ensureAlbumReplicas(ctx context.Context, a domain.LogicalAlbum) error {
	reps, err := r.DB.AlbumReplicas(a.ID)
	if err != nil {
		return err
	}
	for _, m := range r.C.Members {
		if reps[m.ID] != "" {
			continue
		}
		made, err := r.API.CreateAlbum(ctx, m, a.ID, a.Name, a.Description)
		if err != nil {
			return fmt.Errorf("create album for %s: %w", m.ID, err)
		}
		if made.OwnerID != "" && made.OwnerID != m.UserID {
			return errors.New("created album owner mismatch")
		}
		if err := r.DB.SetAlbumReplica(a.ID, m.ID, made.ID); err != nil {
			return err
		}
	}
	return nil
}

type observed struct {
	member  domain.Member
	albumID string
	assets  map[string]bool
}

func (r *Reconciler) Run(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	albums, err := r.DB.Albums()
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range albums {
		if e := r.runAlbum(ctx, a); e != nil {
			r.Log.Error("reconciliation failed", "logical_album_id", a.ID, "error", e)
			errs = append(errs, fmt.Errorf("album %s: %w", a.ID, e))
		}
	}
	if e := r.DB.MarkUnused(); e != nil {
		errs = append(errs, e)
	}
	return errors.Join(errs...)
}

func (r *Reconciler) runAlbum(ctx context.Context, a domain.LogicalAlbum) error {
	if err := r.ensureAlbumReplicas(ctx, a); err != nil {
		return err
	}
	reps, err := r.DB.AlbumReplicas(a.ID)
	if err != nil {
		return err
	}
	obs := []observed{}
	adds := map[string]bool{}
	removes := map[string]bool{}
	for _, m := range r.C.Members {
		albumID := reps[m.ID]
		if albumID == "" {
			return fmt.Errorf("missing album replica for %s", m.ID)
		}
		remote, err := r.API.GetAlbum(ctx, m, albumID)
		if err != nil {
			return fmt.Errorf("get album for %s: %w", m.ID, err)
		}
		if remote.OwnerID != "" && remote.OwnerID != m.UserID {
			return fmt.Errorf("album owner mismatch for %s", m.ID)
		}
		assets, err := r.API.ListAlbumAssets(ctx, m, albumID)
		if err != nil {
			return fmt.Errorf("list album for %s: %w", m.ID, err)
		}
		prior, err := r.DB.Observations(a.ID, m.ID)
		if err != nil {
			return err
		}
		current := map[string]bool{}
		for _, asset := range assets {
			current[asset.ID] = true
			lid, known, e := r.DB.FindReplicaAsset(m.ID, asset.ID)
			if e != nil {
				return e
			}
			if !known {
				if asset.OwnerID != m.UserID || asset.LibraryID == m.LibraryID {
					return fmt.Errorf("unknown bridge or foreign asset %s in %s album", asset.ID, m.ID)
				}
				lid, e = r.DB.EnsureOrigin(r.C.FamilyID, m.ID, asset)
				if e != nil {
					return e
				}
			}
			if !a.Initialized || !prior[asset.ID] {
				adds[lid] = true
			}
		}
		if a.Initialized {
			for old := range prior {
				if !current[old] {
					_, checkErr := r.API.GetAsset(ctx, m, old)
					if errors.Is(checkErr, immich.ErrNotFound) {
						r.Log.Warn("asset missing from Immich; keeping sharing reference", "member_id", m.ID, "immich_asset_id", old)
						continue
					}
					if checkErr != nil {
						return fmt.Errorf("check missing album asset: %w", checkErr)
					}
					lid, known, e := r.DB.FindReplicaAsset(m.ID, old)
					if e != nil {
						return e
					}
					if known {
						removes[lid] = true
					}
				}
			}
		}
		obs = append(obs, observed{member: m, albumID: albumID, assets: current})
	}
	for id := range adds {
		if err := r.DB.SetMembership(a.ID, id, true); err != nil {
			return err
		}
	}
	for id := range removes {
		if !adds[id] {
			if err := r.DB.SetMembership(a.ID, id, false); err != nil {
				return err
			}
		}
	}
	for _, o := range obs {
		if err := r.DB.SetObservations(a.ID, o.member.ID, o.assets); err != nil {
			return err
		}
	}
	if !a.Initialized {
		if err := r.DB.SetInitialized(a.ID); err != nil {
			return err
		}
	}
	desired, err := r.DB.Memberships(a.ID)
	if err != nil {
		return err
	}
	for _, o := range obs {
		for logicalID := range desired {
			rep, err := r.ensureReplica(ctx, logicalID, o.member)
			if err != nil {
				r.Log.Warn("asset replica pending", "logical_asset_id", logicalID, "member_id", o.member.ID, "error", err)
				continue
			}
			if rep.AssetID != "" && !o.assets[rep.AssetID] {
				if err := r.API.AddAssets(ctx, o.member, o.albumID, []string{rep.AssetID}); err != nil {
					return err
				}
				o.assets[rep.AssetID] = true
			}
		}
		coverID := ""
		if a.CoverID != "" {
			cover, found, err := r.DB.Replica(a.CoverID, o.member.ID)
			if err != nil {
				return err
			}
			if found && cover.AssetID != "" && cover.State == "ready" {
				coverID = cover.AssetID
			}
		}
		if err := r.API.UpdateAlbum(ctx, o.member, o.albumID, a.Name, a.Description, coverID); err != nil {
			return err
		}
		for assetID := range o.assets {
			lid, known, err := r.DB.FindReplicaAsset(o.member.ID, assetID)
			if err != nil {
				return err
			}
			if known && !desired[lid] {
				if err := r.API.RemoveAssets(ctx, o.member, o.albumID, []string{assetID}); err != nil {
					return err
				}
				delete(o.assets, assetID)
			}
		}
		if err := r.DB.SetObservations(a.ID, o.member.ID, o.assets); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) ensureReplica(ctx context.Context, logicalID string, m domain.Member) (domain.Replica, error) {
	current, found, err := r.DB.Replica(logicalID, m.ID)
	if err != nil {
		return current, err
	}
	wasReady := found && current.AssetID != "" && current.State == "ready"
	logical, err := r.DB.LogicalAsset(logicalID)
	if err != nil {
		return current, err
	}
	if logical.OriginMember == m.ID {
		return current, errors.New("missing origin replica mapping")
	}
	origin, ok := r.member(logical.OriginMember)
	if !ok {
		return current, errors.New("origin member missing")
	}
	asset, err := r.API.GetAsset(ctx, origin, logical.OriginAsset)
	if err != nil {
		if !found {
			current = domain.Replica{LogicalID: logicalID, MemberID: m.ID, Role: "external_replica", LibraryID: m.LibraryID}
		}
		current.State = "api_error"
		if errors.Is(err, immich.ErrNotFound) {
			current.State = "source_missing"
		}
		current.Error = err.Error()
		_ = r.DB.UpsertReplica(current)
		return current, fmt.Errorf("source asset unavailable: %w", err)
	}
	if asset.OwnerID != origin.UserID {
		return current, errors.New("source owner changed")
	}
	if !asset.Supported() {
		if !found {
			current = domain.Replica{LogicalID: logicalID, MemberID: m.ID, Role: "external_replica", LibraryID: m.LibraryID}
		}
		current.State = "unsupported"
		current.Error = "coupled or unsupported media type"
		_ = r.DB.UpsertReplica(current)
		return current, errors.New(current.Error)
	}
	path, err := r.FS.Destination(r.C.FamilyID, m.ID, logical.OriginMember, logical.OriginAsset, asset.OriginalPath)
	if err != nil {
		return current, err
	}
	if found && current.Path != "" && current.Path != path {
		return current, errors.New("stored recipient path differs from deterministic path")
	}
	if wasReady {
		remote, e := r.API.GetAsset(ctx, m, current.AssetID)
		if e == nil && remote.OwnerID == m.UserID && remote.LibraryID == m.LibraryID {
			if e := r.FS.Ensure(asset.OriginalPath, path); e == nil {
				return current, nil
			}
		} else if e != nil {
			r.Log.Warn("recipient Immich asset missing; rediscovering", "logical_asset_id", logicalID, "member_id", m.ID, "error", e)
		}
	}
	current = domain.Replica{LogicalID: logicalID, MemberID: m.ID, Role: "external_replica", Path: path, LibraryID: m.LibraryID, State: "pending_link"}
	if err := r.DB.UpsertReplica(current); err != nil {
		return current, err
	}
	if err := r.FS.Ensure(asset.OriginalPath, path); err != nil {
		current.State = "error"
		current.Error = err.Error()
		_ = r.DB.UpsertReplica(current)
		return current, err
	}
	current.State = "pending_import"
	if err := r.DB.UpsertReplica(current); err != nil {
		return current, err
	}
	if err := r.API.ScanLibrary(ctx, m); err != nil {
		return current, fmt.Errorf("scan library: %w", err)
	}
	rel, err := filepath.Rel(r.C.BridgeRoot, path)
	if err != nil {
		return current, err
	}
	remotePath := filepath.Join(r.C.ImmichBridgeRoot, rel)
	if !strings.HasPrefix(remotePath, r.C.ImmichBridgeRoot+string(filepath.Separator)) {
		return current, errors.New("invalid remote bridge path")
	}
	foundAssets, err := r.API.FindByPath(ctx, m, remotePath)
	if err != nil {
		return current, err
	}
	if len(foundAssets) != 1 {
		return current, fmt.Errorf("pending import: expected one matching asset, got %d", len(foundAssets))
	}
	if foundAssets[0].OwnerID != m.UserID || foundAssets[0].LibraryID != m.LibraryID || foundAssets[0].OriginalPath != remotePath {
		return current, errors.New("imported asset identity mismatch")
	}
	current.AssetID = foundAssets[0].ID
	current.State = "ready"
	current.Error = ""
	return current, r.DB.UpsertReplica(current)
}

type Action struct {
	Kind           string `json:"kind"`
	AlbumID        string `json:"albumId"`
	MemberID       string `json:"memberId,omitempty"`
	LogicalAssetID string `json:"logicalAssetId,omitempty"`
	ImmichAssetID  string `json:"immichAssetId,omitempty"`
}

func (r *Reconciler) DryRun(ctx context.Context) (map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	albums, err := r.DB.Albums()
	if err != nil {
		return nil, err
	}
	actions := []Action{}
	for _, album := range albums {
		reps, err := r.DB.AlbumReplicas(album.ID)
		if err != nil {
			return nil, err
		}
		desired, err := r.DB.Memberships(album.ID)
		if err != nil {
			return nil, err
		}
		adds := map[string]bool{}
		removes := map[string]bool{}
		actual := map[string]map[string]bool{}
		for _, m := range r.C.Members {
			albumID := reps[m.ID]
			if albumID == "" {
				actions = append(actions, Action{Kind: "create_album_replica", AlbumID: album.ID, MemberID: m.ID})
				continue
			}
			assets, err := r.API.ListAlbumAssets(ctx, m, albumID)
			if err != nil {
				return nil, err
			}
			prior, err := r.DB.Observations(album.ID, m.ID)
			if err != nil {
				return nil, err
			}
			actual[m.ID] = map[string]bool{}
			for _, asset := range assets {
				actual[m.ID][asset.ID] = true
				lid, known, e := r.DB.FindReplicaAsset(m.ID, asset.ID)
				if e != nil {
					return nil, e
				}
				if !known {
					if asset.OwnerID != m.UserID || asset.LibraryID == m.LibraryID {
						actions = append(actions, Action{Kind: "mapping_inconsistency", AlbumID: album.ID, MemberID: m.ID, ImmichAssetID: asset.ID})
						continue
					}
					actions = append(actions, Action{Kind: "discover_origin", AlbumID: album.ID, MemberID: m.ID, ImmichAssetID: asset.ID})
					for _, other := range r.C.Members {
						if other.ID != m.ID {
							actions = append(actions, Action{Kind: "link_and_import", AlbumID: album.ID, MemberID: other.ID, ImmichAssetID: asset.ID})
						}
					}
					continue
				}
				if !album.Initialized || !prior[asset.ID] {
					adds[lid] = true
				}
			}
			if album.Initialized {
				for old := range prior {
					if !actual[m.ID][old] {
						lid, known, e := r.DB.FindReplicaAsset(m.ID, old)
						if e != nil {
							return nil, e
						}
						if known {
							removes[lid] = true
						}
					}
				}
			}
		}
		for id := range adds {
			if !desired[id] {
				actions = append(actions, Action{Kind: "add_sharing_reference", AlbumID: album.ID, LogicalAssetID: id})
			}
			desired[id] = true
		}
		for id := range removes {
			if !adds[id] {
				delete(desired, id)
				actions = append(actions, Action{Kind: "remove_sharing_reference", AlbumID: album.ID, LogicalAssetID: id})
			}
		}
		for _, m := range r.C.Members {
			if reps[m.ID] == "" {
				continue
			}
			for id := range desired {
				rep, found, e := r.DB.Replica(id, m.ID)
				if e != nil {
					return nil, e
				}
				if !found || rep.AssetID == "" {
					actions = append(actions, Action{Kind: "link_and_import", AlbumID: album.ID, MemberID: m.ID, LogicalAssetID: id})
				} else if !actual[m.ID][rep.AssetID] {
					actions = append(actions, Action{Kind: "add_album_asset", AlbumID: album.ID, MemberID: m.ID, LogicalAssetID: id, ImmichAssetID: rep.AssetID})
				}
			}
			for assetID := range actual[m.ID] {
				lid, known, e := r.DB.FindReplicaAsset(m.ID, assetID)
				if e != nil {
					return nil, e
				}
				if known && !desired[lid] {
					actions = append(actions, Action{Kind: "remove_album_asset", AlbumID: album.ID, MemberID: m.ID, LogicalAssetID: lid, ImmichAssetID: assetID})
				}
			}
		}
	}
	return map[string]any{"actions": actions, "count": len(actions)}, nil
}

func (r *Reconciler) Check(ctx context.Context) error {
	for _, m := range r.C.Members {
		id, err := r.API.Me(ctx, m)
		if err != nil {
			return err
		}
		if id != m.UserID {
			return fmt.Errorf("member %s API key belongs to %s", m.ID, id)
		}
	}
	return nil
}
