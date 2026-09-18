package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

var ErrDryRunMode = errors.New("dry-run mode enabled; proposed actions are reported in the service logs")
var errPendingImport = errors.New("waiting for Immich library import")

func New(c config.Config, db *store.Store, api immich.Client, log *slog.Logger) *Reconciler {
	return &Reconciler{C: c, DB: db, API: api, FS: filesystem.Linker{SourceRoot: c.SourceRoot, BridgeRoot: c.BridgeRoot, ReadOnly: c.DryRun}, Log: log}
}
func (r *Reconciler) member(id string) (domain.Member, bool) {
	for _, m := range r.C.Members {
		if m.ID == id {
			return m, true
		}
	}
	return domain.Member{}, false
}

// EnsureTogether keeps one logical catch-all album and adopts uniquely named
// member albums when they already exist. Immich writes remain in Run.
func (r *Reconciler) EnsureTogether(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := r.C.TogetherAlbumName
	if name == "" {
		name = "Together"
	}
	albums, err := r.DB.Albums()
	if err != nil {
		return "", err
	}
	var selected *domain.LogicalAlbum
	for i := range albums {
		if albums[i].SystemKey == "together" {
			selected = &albums[i]
			break
		}
	}
	if selected == nil {
		for i := range albums {
			if albums[i].Name != name {
				continue
			}
			if selected != nil {
				return "", fmt.Errorf("multiple registered albums named %q; cannot choose a Together album", name)
			}
			selected = &albums[i]
		}
	}
	replicas := map[string]string{}
	if selected != nil {
		replicas, err = r.DB.AlbumReplicas(selected.ID)
		if err != nil {
			return "", err
		}
	}
	found := map[string]string{}
	description := ""
	for _, m := range r.C.Members {
		if replicas[m.ID] != "" {
			continue
		}
		album, exists, err := r.findOwnedAlbumByName(ctx, m, name)
		if err != nil {
			return "", err
		}
		if !exists {
			continue
		}
		mapped, known, err := r.DB.AlbumByReplica(m.ID, album.ID)
		if err != nil {
			return "", err
		}
		if known && (selected == nil || mapped != selected.ID) {
			return "", fmt.Errorf("%s album %s is already registered to another logical album", m.ID, album.ID)
		}
		found[m.ID] = album.ID
		if description == "" {
			description = album.Description
		}
	}
	if selected == nil {
		album := domain.LogicalAlbum{ID: store.ID(), Name: name, Description: description, SystemKey: "together"}
		if err := r.DB.AddAlbum(r.C.FamilyID, album, found); err != nil {
			return "", err
		}
		return album.ID, nil
	}
	if selected.SystemKey == "" {
		if err := r.DB.SetAlbumSystemKey(selected.ID, "together"); err != nil {
			return "", err
		}
	}
	if selected.Name != name {
		if err := r.DB.UpdateAlbum(selected.ID, name, selected.Description, selected.CoverID); err != nil {
			return "", err
		}
	}
	for memberID, albumID := range found {
		if err := r.DB.SetAlbumReplica(selected.ID, memberID, albumID); err != nil {
			return "", err
		}
	}
	return selected.ID, nil
}

func (r *Reconciler) findOwnedAlbumByName(ctx context.Context, m domain.Member, name string) (domain.Album, bool, error) {
	albums, err := r.API.ListAlbums(ctx, m)
	if err != nil {
		return domain.Album{}, false, fmt.Errorf("list albums for %s: %w", m.ID, err)
	}
	var match domain.Album
	for _, album := range albums {
		if album.Name != name {
			continue
		}
		if match.ID != "" {
			return domain.Album{}, false, fmt.Errorf("member %s has multiple albums named %q", m.ID, name)
		}
		match = album
	}
	if match.ID == "" {
		return domain.Album{}, false, nil
	}
	match, err = r.API.GetAlbum(ctx, m, match.ID)
	if err != nil {
		return domain.Album{}, false, err
	}
	if match.OwnerID != m.UserID || match.Name != name {
		return domain.Album{}, false, fmt.Errorf("member %s album ownership or name changed during lookup", m.ID)
	}
	return match, true, nil
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
				if r.C.DryRun {
					return id, nil
				}
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
			if asset, e = r.mappedAsset(asset); e == nil {
				if lid, e := r.DB.EnsureOrigin(r.C.FamilyID, m.ID, asset); e == nil {
					_ = r.DB.SetCover(id, lid)
				}
			}
		}
	}
	if !r.C.DryRun {
		if err := r.ensureAlbumReplicas(ctx, logical); err != nil {
			return id, err
		}
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
		if a.SystemKey == "together" {
			existing, found, err := r.findOwnedAlbumByName(ctx, m, a.Name)
			if err != nil {
				return err
			}
			if found {
				if err := r.DB.SetAlbumReplica(a.ID, m.ID, existing.ID); err != nil {
					return err
				}
				continue
			}
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
	if r.C.DryRun {
		return ErrDryRunMode
	}
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
				asset, e = r.mappedAsset(asset)
				if e != nil {
					return fmt.Errorf("source path for asset %s: %w", asset.ID, e)
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
	if a.CoverID != "" && !desired[a.CoverID] {
		if err := r.DB.SetCover(a.ID, ""); err != nil {
			return err
		}
		a.CoverID = ""
	}
	for _, o := range obs {
		for logicalID := range desired {
			rep, err := r.ensureReplica(ctx, logicalID, o.member)
			if err != nil {
				if errors.Is(err, errPendingImport) {
					r.Log.Info("asset waiting for Immich library import", "logical_asset_id", logicalID, "member_id", o.member.ID, "state", "pending_import")
					continue
				}
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
		if !found || current.Role != "origin" || current.AssetID != logical.OriginAsset {
			return current, errors.New("missing origin replica mapping")
		}
		originAsset, e := r.API.GetAsset(ctx, m, current.AssetID)
		if e != nil {
			current.State = "api_error"
			if errors.Is(e, immich.ErrNotFound) {
				current.State = "source_missing"
			}
			current.Error = e.Error()
			_ = r.DB.UpsertReplica(current)
			return current, e
		}
		path, e := r.C.SourcePath(originAsset.OriginalPath)
		if e != nil || originAsset.OwnerID != m.UserID {
			return current, errors.New("origin asset path or owner changed")
		}
		if path != current.Path {
			if e := r.rebindOriginPath(logicalID, m.ID, current.Path, path); e != nil {
				return current, fmt.Errorf("origin path changed: %w", e)
			}
			current.Path = path
			r.Log.Info("origin path updated after same-inode move", "logical_asset_id", logicalID, "member_id", m.ID)
		}
		if current.State != "ready" || current.Error != "" {
			current.State, current.Error = "ready", ""
			if e := r.DB.UpsertReplica(current); e != nil {
				return current, e
			}
		}
		return current, nil
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
	asset, err = r.mappedAsset(asset)
	if err != nil {
		return current, err
	}
	path, err := r.FS.Destination(r.C.FamilyID, m.ID, logical.OriginMember, logical.OriginAsset, asset.OriginalPath)
	if err != nil {
		return current, err
	}
	rel, err := filepath.Rel(r.C.BridgeRoot, path)
	if err != nil {
		return current, err
	}
	remotePath := filepath.Join(r.C.ImmichBridgeRoot, rel)
	if !strings.HasPrefix(remotePath, r.C.ImmichBridgeRoot+string(filepath.Separator)) {
		return current, errors.New("invalid remote bridge path")
	}
	if found && current.Path != "" && current.Path != path {
		return current, errors.New("stored recipient path differs from deterministic path")
	}
	if wasReady {
		remote, e := r.API.GetAsset(ctx, m, current.AssetID)
		if e == nil && (remote.OwnerID != m.UserID || remote.LibraryID != m.LibraryID || remote.OriginalPath != remotePath) {
			return current, errors.New("stored recipient asset identity mismatch")
		}
		if e == nil {
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
	foundAssets, err := r.API.FindByPath(ctx, m, remotePath)
	if err != nil {
		return current, err
	}
	if len(foundAssets) == 0 {
		return current, errPendingImport
	}
	if len(foundAssets) != 1 {
		return current, fmt.Errorf("expected one imported asset at %s, got %d", remotePath, len(foundAssets))
	}
	if foundAssets[0].OwnerID != m.UserID || foundAssets[0].LibraryID != m.LibraryID || foundAssets[0].OriginalPath != remotePath {
		return current, errors.New("imported asset identity mismatch")
	}
	current.AssetID = foundAssets[0].ID
	current.State = "ready"
	current.Error = ""
	if err := r.DB.UpsertReplica(current); err != nil {
		return current, err
	}
	r.Log.Info("asset replica ready", "logical_asset_id", logicalID, "member_id", m.ID, "immich_asset_id", current.AssetID)
	return current, nil
}

type Action struct {
	Kind           string
	AlbumID        string
	AlbumName      string
	MemberID       string
	SourceMemberID string
	LogicalAssetID string
	ImmichAssetID  string
	Error          string
}

func (a Action) Message() string {
	switch a.Kind {
	case "create_album_replica":
		return "dry-run: would create album for member"
	case "mapping_inconsistency":
		return "dry-run: unknown bridge or foreign asset needs review"
	case "unsupported_asset":
		return "dry-run: source asset cannot be shared"
	case "source_error":
		return "dry-run: source asset path needs review"
	case "source_missing":
		return "dry-run: previously observed asset is missing from Immich; sharing reference would be kept"
	case "discover_origin":
		return "dry-run: new source asset found in album"
	case "link_and_import":
		return "dry-run: would share asset with member through hardlink and import"
	case "add_sharing_reference":
		return "dry-run: would add asset to shared album membership"
	case "remove_sharing_reference":
		return "dry-run: would remove album sharing reference"
	case "add_album_asset":
		return "dry-run: would add member asset to album"
	case "remove_album_asset":
		return "dry-run: would remove member asset from album"
	case "update_album_metadata":
		return "dry-run: would update member album name or description"
	case "update_album_cover":
		return "dry-run: would update member album cover"
	default:
		return "dry-run: proposed action"
	}
}

func (r *Reconciler) DryRun(ctx context.Context) ([]Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	albums, err := r.DB.Albums()
	if err != nil {
		return nil, err
	}
	actions := []Action{}
	for _, album := range albums {
		add := func(action Action) {
			action.AlbumID = album.ID
			action.AlbumName = album.Name
			actions = append(actions, action)
		}
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
				add(Action{Kind: "create_album_replica", MemberID: m.ID})
				continue
			}
			remoteAlbum, err := r.API.GetAlbum(ctx, m, albumID)
			if err != nil {
				return nil, fmt.Errorf("get album for %s: %w", m.ID, err)
			}
			if remoteAlbum.OwnerID != "" && remoteAlbum.OwnerID != m.UserID {
				add(Action{Kind: "mapping_inconsistency", MemberID: m.ID, Error: "album owner mismatch"})
				continue
			}
			if remoteAlbum.Name != album.Name || remoteAlbum.Description != album.Description {
				add(Action{Kind: "update_album_metadata", MemberID: m.ID})
			}
			if album.CoverID != "" {
				cover, found, err := r.DB.Replica(album.CoverID, m.ID)
				if err != nil {
					return nil, err
				}
				if found && cover.AssetID != "" && cover.State == "ready" && remoteAlbum.CoverID != cover.AssetID {
					add(Action{Kind: "update_album_cover", MemberID: m.ID, LogicalAssetID: album.CoverID, ImmichAssetID: cover.AssetID})
				}
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
						add(Action{Kind: "mapping_inconsistency", MemberID: m.ID, ImmichAssetID: asset.ID})
						continue
					}
					sourceID := asset.ID
					asset, e = r.API.GetAsset(ctx, m, sourceID)
					if e != nil {
						return nil, fmt.Errorf("inspect source asset %s: %w", sourceID, e)
					}
					if asset.OwnerID != m.UserID || asset.LibraryID == m.LibraryID {
						add(Action{Kind: "mapping_inconsistency", MemberID: m.ID, ImmichAssetID: sourceID})
						continue
					}
					if !asset.Supported() {
						add(Action{Kind: "unsupported_asset", MemberID: m.ID, ImmichAssetID: asset.ID})
						continue
					}
					mapped, e := r.mappedAsset(asset)
					if e == nil {
						_, e = r.FS.SourceInfo(mapped.OriginalPath)
					}
					if e != nil {
						add(Action{Kind: "source_error", MemberID: m.ID, ImmichAssetID: asset.ID, Error: e.Error()})
						continue
					}
					add(Action{Kind: "discover_origin", MemberID: m.ID, ImmichAssetID: asset.ID})
					for _, other := range r.C.Members {
						if other.ID != m.ID {
							add(Action{Kind: "link_and_import", MemberID: other.ID, SourceMemberID: m.ID, ImmichAssetID: asset.ID})
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
						_, checkErr := r.API.GetAsset(ctx, m, old)
						if errors.Is(checkErr, immich.ErrNotFound) {
							add(Action{Kind: "source_missing", MemberID: m.ID, ImmichAssetID: old})
							continue
						}
						if checkErr != nil {
							return nil, fmt.Errorf("check missing album asset for %s: %w", m.ID, checkErr)
						}
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
				add(Action{Kind: "add_sharing_reference", LogicalAssetID: id})
			}
			desired[id] = true
		}
		for id := range removes {
			if !adds[id] {
				wasDesired := desired[id]
				delete(desired, id)
				if wasDesired {
					add(Action{Kind: "remove_sharing_reference", LogicalAssetID: id})
				}
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
					add(Action{Kind: "link_and_import", MemberID: m.ID, LogicalAssetID: id})
				} else if !actual[m.ID][rep.AssetID] {
					add(Action{Kind: "add_album_asset", MemberID: m.ID, LogicalAssetID: id, ImmichAssetID: rep.AssetID})
				}
			}
			for assetID := range actual[m.ID] {
				lid, known, e := r.DB.FindReplicaAsset(m.ID, assetID)
				if e != nil {
					return nil, e
				}
				if known && !desired[lid] {
					add(Action{Kind: "remove_album_asset", MemberID: m.ID, LogicalAssetID: lid, ImmichAssetID: assetID})
				}
			}
		}
	}
	sort.Slice(actions, func(i, j int) bool {
		a, b := actions[i], actions[j]
		left := [...]string{a.AlbumID, a.Kind, a.MemberID, a.SourceMemberID, a.LogicalAssetID, a.ImmichAssetID}
		right := [...]string{b.AlbumID, b.Kind, b.MemberID, b.SourceMemberID, b.LogicalAssetID, b.ImmichAssetID}
		for k := range left {
			if left[k] != right[k] {
				return left[k] < right[k]
			}
		}
		return false
	})
	return actions, nil
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
		library, err := r.API.GetLibrary(ctx, m)
		if err != nil {
			return fmt.Errorf("member %s library: %w", m.ID, err)
		}
		expected := filepath.Join(r.C.ImmichBridgeRoot, "families", r.C.FamilyID, "users", m.ID, "assets")
		if library.ID != m.LibraryID || library.OwnerID != m.UserID || len(library.ImportPaths) != 1 || library.ImportPaths[0] != expected {
			return fmt.Errorf("member %s library identity or import path mismatch", m.ID)
		}
	}
	return nil
}

func (r *Reconciler) mappedAsset(asset domain.Asset) (domain.Asset, error) {
	path, err := r.C.SourcePath(asset.OriginalPath)
	if err != nil {
		return asset, err
	}
	asset.OriginalPath = path
	return asset, nil
}

func (r *Reconciler) rebindOriginPath(logicalID, memberID, oldPath, newPath string) error {
	newInfo, err := r.FS.SourceInfo(newPath)
	if err != nil {
		return err
	}
	anchored := false
	oldInfo, err := r.FS.SourceInfo(oldPath)
	if err == nil {
		if !os.SameFile(oldInfo, newInfo) {
			return errors.New("new source has a different inode")
		}
		anchored = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	replicas, err := r.DB.ReplicasFor(logicalID)
	if err != nil {
		return err
	}
	for _, rep := range replicas {
		if rep.Role != "external_replica" || rep.Path == "" {
			continue
		}
		info, err := os.Lstat(rep.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !os.SameFile(info, newInfo) {
			return errors.New("recipient link does not match moved source inode")
		}
		if err := r.FS.Ensure(newPath, rep.Path); err != nil {
			return err
		}
		anchored = true
	}
	if !anchored {
		return errors.New("cannot verify moved source inode from existing links")
	}
	return r.DB.UpdateOriginPath(logicalID, memberID, oldPath, newPath)
}
