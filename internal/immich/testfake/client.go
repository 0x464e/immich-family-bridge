package testfake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/0x464e/immich-family-bridge/internal/config"
	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
)

type State struct {
	Assets     map[string]domain.Asset    `json:"assets"`
	Albums     map[string]domain.Album    `json:"albums"`
	Membership map[string]map[string]bool `json:"membership"`
	Stacks     map[string]fakeStack       `json:"stacks"`
}

type fakeStack struct {
	ID, OwnerID, PrimaryAssetID string
	AssetIDs                    []string
}

// Seed is used only by unit tests. The running service never constructs this client.
type Seed struct {
	Assets []SeedAsset
	Albums []SeedAlbum
}
type SeedAsset struct {
	ID, Member, Path, Type, SidecarPath string
}
type SeedAlbum struct {
	ID, Member, Name, Description string
	AssetIDs                      []string
}
type Client struct {
	mu          sync.Mutex
	config      config.Config
	statePath   string
	state       State
	scanBlocked bool
	failNext    bool
}

func New(c config.Config, statePath string, data Seed) (*Client, error) {
	f := &Client{config: c, statePath: statePath, state: State{Assets: map[string]domain.Asset{}, Albums: map[string]domain.Album{}, Membership: map[string]map[string]bool{}, Stacks: map[string]fakeStack{}}}
	seed := true
	if b, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(b, &f.state); err != nil {
			return nil, err
		}
		seed = false
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if f.state.Assets == nil {
		f.state.Assets = map[string]domain.Asset{}
	}
	if f.state.Albums == nil {
		f.state.Albums = map[string]domain.Album{}
	}
	if f.state.Membership == nil {
		f.state.Membership = map[string]map[string]bool{}
	}
	if f.state.Stacks == nil {
		f.state.Stacks = map[string]fakeStack{}
	}
	for _, a := range data.Assets {
		if !seed {
			continue
		}
		m, ok := f.member(a.Member)
		if !ok {
			return nil, errors.New("unknown fake member")
		}
		typ := a.Type
		if typ == "" {
			typ = "IMAGE"
		}
		f.state.Assets[a.ID] = domain.Asset{ID: a.ID, OwnerID: m.UserID, OriginalPath: a.Path, OriginalFileName: filepath.Base(a.Path), Type: typ, Sidecar: a.SidecarPath != "", SidecarPath: a.SidecarPath}
	}
	for _, a := range data.Albums {
		m, _ := f.member(a.Member)
		if seed {
			f.state.Albums[a.ID] = domain.Album{ID: a.ID, OwnerID: m.UserID, Name: a.Name, Description: a.Description}
		}
		if f.state.Membership[a.ID] == nil {
			f.state.Membership[a.ID] = map[string]bool{}
		}
		if seed {
			for _, id := range a.AssetIDs {
				f.state.Membership[a.ID][id] = true
			}
		}
	}
	return f, nil
}
func (f *Client) DeleteAsset(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.state.Assets, id)
	for _, members := range f.state.Membership {
		delete(members, id)
	}
	return f.save()
}
func (f *Client) MoveAssetPath(id, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	asset, ok := f.state.Assets[id]
	if !ok {
		return immich.ErrNotFound
	}
	asset.OriginalPath = path
	f.state.Assets[id] = asset
	return f.save()
}
func (f *Client) SetSidecar(id, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	asset, ok := f.state.Assets[id]
	if !ok {
		return immich.ErrNotFound
	}
	asset.Sidecar = path != ""
	asset.SidecarPath = path
	f.state.Assets[id] = asset
	return f.save()
}

func (f *Client) AddAsset(id, memberID, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.member(memberID)
	if !ok {
		return errors.New("unknown fake member")
	}
	f.state.Assets[id] = domain.Asset{ID: id, OwnerID: m.UserID, OriginalPath: path, OriginalFileName: filepath.Base(path), Type: "IMAGE"}
	return f.save()
}

// SetStack is a test helper that makes the first supplied source asset the
// primary member of a source stack.
func (f *Client) SetStack(memberID, stackID string, assetIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.member(memberID)
	if !ok || len(assetIDs) < 2 {
		return errors.New("invalid fake stack")
	}
	if prior, found := f.state.Stacks[stackID]; found {
		for _, id := range prior.AssetIDs {
			if a, exists := f.state.Assets[id]; exists && a.StackID == stackID {
				a.StackID, a.StackPrimaryID = "", ""
				f.state.Assets[id] = a
			}
		}
	}
	for _, id := range assetIDs {
		a, found := f.state.Assets[id]
		if !found || a.OwnerID != m.UserID {
			return errors.New("fake stack asset owner mismatch")
		}
		a.StackID, a.StackPrimaryID = stackID, assetIDs[0]
		f.state.Assets[id] = a
	}
	f.state.Stacks[stackID] = fakeStack{ID: stackID, OwnerID: m.UserID, PrimaryAssetID: assetIDs[0], AssetIDs: append([]string(nil), assetIDs...)}
	return f.save()
}
func (f *Client) member(id string) (domain.Member, bool) {
	for _, m := range f.config.Members {
		if m.ID == id {
			return m, true
		}
	}
	return domain.Member{}, false
}
func (f *Client) save() error {
	if err := os.MkdirAll(filepath.Dir(f.statePath), 0750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0640); err != nil {
		return err
	}
	return os.Rename(tmp, f.statePath)
}
func (f *Client) fail() error {
	if f.failNext {
		f.failNext = false
		return errors.New("fake transient API outage")
	}
	return nil
}
func (f *Client) FailNext()                               { f.mu.Lock(); defer f.mu.Unlock(); f.failNext = true }
func (f *Client) BlockScans(v bool)                       { f.mu.Lock(); defer f.mu.Unlock(); f.scanBlocked = v }
func (f *Client) Version(context.Context) (string, error) { return "3.2.0-fake", nil }
func (f *Client) Me(_ context.Context, m domain.Member) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return "", err
	}
	return m.UserID, nil
}
func (f *Client) Permissions(context.Context, domain.Member) ([]string, error) {
	return []string{"all"}, nil
}
func (f *Client) GetLibrary(_ context.Context, m domain.Member) (domain.Library, error) {
	path := filepath.Join(f.config.ImmichBridgeRoot, "families", f.config.FamilyID, "users", m.ID, "assets")
	return domain.Library{ID: m.LibraryID, OwnerID: m.UserID, ImportPaths: []string{path}}, nil
}
func (f *Client) GetAsset(_ context.Context, m domain.Member, id string) (domain.Asset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return domain.Asset{}, err
	}
	a, ok := f.state.Assets[id]
	if !ok || a.OwnerID != m.UserID {
		return a, immich.ErrNotFound
	}
	return a, nil
}

func (f *Client) GetStack(_ context.Context, m domain.Member, id string) (domain.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return domain.Stack{}, err
	}
	stack, ok := f.state.Stacks[id]
	if !ok || stack.OwnerID != m.UserID {
		return domain.Stack{}, immich.ErrNotFound
	}
	out := domain.Stack{ID: stack.ID, OwnerID: stack.OwnerID, PrimaryAssetID: stack.PrimaryAssetID}
	for _, assetID := range stack.AssetIDs {
		if asset, ok := f.state.Assets[assetID]; ok {
			out.Assets = append(out.Assets, asset)
		}
	}
	return out, nil
}
func (f *Client) GetAlbum(_ context.Context, m domain.Member, id string) (domain.Album, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return domain.Album{}, err
	}
	a, ok := f.state.Albums[id]
	if !ok || a.OwnerID != m.UserID {
		return a, immich.ErrNotFound
	}
	return a, nil
}
func (f *Client) ListAlbums(_ context.Context, m domain.Member) ([]domain.Album, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	out := []domain.Album{}
	for _, a := range f.state.Albums {
		if a.OwnerID == m.UserID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (f *Client) ListAlbumAssets(_ context.Context, m domain.Member, id string) ([]domain.Asset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	a, ok := f.state.Albums[id]
	if !ok || a.OwnerID != m.UserID {
		return nil, os.ErrNotExist
	}
	out := []domain.Asset{}
	for aid := range f.state.Membership[id] {
		asset, ok := f.state.Assets[aid]
		if ok && asset.OwnerID == m.UserID {
			out = append(out, asset)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (f *Client) CreateAlbum(_ context.Context, m domain.Member, logicalID, name, desc string) (domain.Album, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return domain.Album{}, err
	}
	sum := sha256.Sum256([]byte(m.UserID + "\x00" + logicalID))
	id := "fake-album-" + hex.EncodeToString(sum[:8])
	if a, ok := f.state.Albums[id]; ok {
		return a, nil
	}
	a := domain.Album{ID: id, OwnerID: m.UserID, Name: name, Description: desc}
	f.state.Albums[id] = a
	f.state.Membership[id] = map[string]bool{}
	return a, f.save()
}
func (f *Client) UpdateAlbum(_ context.Context, m domain.Member, id, name, desc, cover string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	a, ok := f.state.Albums[id]
	if !ok || a.OwnerID != m.UserID {
		return os.ErrNotExist
	}
	a.Name = name
	a.Description = desc
	a.CoverID = cover
	f.state.Albums[id] = a
	return f.save()
}
func (f *Client) AddAssets(_ context.Context, m domain.Member, id string, assets []string) error {
	return f.change(m, id, assets, true)
}
func (f *Client) RemoveAssets(_ context.Context, m domain.Member, id string, assets []string) error {
	return f.change(m, id, assets, false)
}
func (f *Client) DeleteAssets(_ context.Context, m domain.Member, assets []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	for _, id := range assets {
		asset, ok := f.state.Assets[id]
		if !ok {
			continue
		}
		if asset.OwnerID != m.UserID || asset.LibraryID != m.LibraryID {
			return fmt.Errorf("asset %s is not in %s recipient library", id, m.ID)
		}
		delete(f.state.Assets, id)
		for _, membership := range f.state.Membership {
			delete(membership, id)
		}
	}
	return f.save()
}

func (f *Client) CreateStack(_ context.Context, m domain.Member, assets []string) (domain.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return domain.Stack{}, err
	}
	if len(assets) < 2 {
		return domain.Stack{}, errors.New("stack requires two assets")
	}
	for _, id := range assets {
		a, ok := f.state.Assets[id]
		if !ok || a.OwnerID != m.UserID {
			return domain.Stack{}, errors.New("fake recipient stack asset owner mismatch")
		}
	}
	sum := sha256.Sum256([]byte(m.UserID + "\x00" + strings.Join(assets, "\x00")))
	id := "fake-stack-" + hex.EncodeToString(sum[:12])
	for oldID, old := range f.state.Stacks {
		if old.OwnerID != m.UserID {
			continue
		}
		for _, oldAsset := range old.AssetIDs {
			for _, newAsset := range assets {
				if oldAsset == newAsset {
					delete(f.state.Stacks, oldID)
					break
				}
			}
		}
	}
	for _, assetID := range assets {
		a := f.state.Assets[assetID]
		a.StackID, a.StackPrimaryID = id, assets[0]
		f.state.Assets[assetID] = a
	}
	f.state.Stacks[id] = fakeStack{ID: id, OwnerID: m.UserID, PrimaryAssetID: assets[0], AssetIDs: append([]string(nil), assets...)}
	if err := f.save(); err != nil {
		return domain.Stack{}, err
	}
	return f.stackDomain(id)
}

func (f *Client) DeleteStack(_ context.Context, m domain.Member, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	stack, ok := f.state.Stacks[id]
	if !ok || stack.OwnerID != m.UserID {
		return immich.ErrNotFound
	}
	for _, assetID := range stack.AssetIDs {
		if asset, ok := f.state.Assets[assetID]; ok && asset.StackID == id {
			asset.StackID, asset.StackPrimaryID = "", ""
			f.state.Assets[assetID] = asset
		}
	}
	delete(f.state.Stacks, id)
	return f.save()
}

func (f *Client) stackDomain(id string) (domain.Stack, error) {
	stack, ok := f.state.Stacks[id]
	if !ok {
		return domain.Stack{}, immich.ErrNotFound
	}
	out := domain.Stack{ID: stack.ID, OwnerID: stack.OwnerID, PrimaryAssetID: stack.PrimaryAssetID}
	for _, assetID := range stack.AssetIDs {
		out.Assets = append(out.Assets, f.state.Assets[assetID])
	}
	return out, nil
}
func (f *Client) change(m domain.Member, id string, assets []string, add bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	a, ok := f.state.Albums[id]
	if !ok || a.OwnerID != m.UserID {
		return os.ErrNotExist
	}
	for _, x := range assets {
		v, ok := f.state.Assets[x]
		if !ok || v.OwnerID != m.UserID {
			return fmt.Errorf("asset %s not owned by %s", x, m.ID)
		}
		f.state.Membership[id][x] = add
		if !add {
			delete(f.state.Membership[id], x)
		}
	}
	return f.save()
}
func (f *Client) ScanLibrary(_ context.Context, m domain.Member) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	if f.scanBlocked {
		return nil
	}
	root := filepath.Join(f.config.BridgeRoot, "families", f.config.FamilyID, "users", m.ID, "assets")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".xmp") {
			return nil
		}
		rel, e := filepath.Rel(f.config.BridgeRoot, path)
		if e != nil {
			return nil
		}
		immichPath := filepath.Join(f.config.ImmichBridgeRoot, rel)
		for _, a := range f.state.Assets {
			if a.OriginalPath == immichPath && a.OwnerID == m.UserID {
				return nil
			}
		}
		sum := sha256.Sum256([]byte(m.UserID + "\x00" + immichPath))
		id := "fake-asset-" + hex.EncodeToString(sum[:12])
		typ := "IMAGE"
		if strings.EqualFold(filepath.Ext(path), ".mp4") {
			typ = "VIDEO"
		}
		asset := domain.Asset{ID: id, OwnerID: m.UserID, LibraryID: m.LibraryID, OriginalPath: immichPath, OriginalFileName: filepath.Base(path), Type: typ}
		for _, candidate := range []string{path + ".xmp", strings.TrimSuffix(path, filepath.Ext(path)) + ".xmp"} {
			if info, err := os.Lstat(candidate); err == nil && info.Mode().IsRegular() {
				relSidecar, err := filepath.Rel(f.config.BridgeRoot, candidate)
				if err == nil {
					asset.Sidecar = true
					asset.SidecarPath = filepath.Join(f.config.ImmichBridgeRoot, relSidecar)
				}
				break
			}
		}
		f.state.Assets[id] = asset
		return nil
	})
	return f.save()
}
func (f *Client) DiscoverSidecars(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return err
	}
	for id, asset := range f.state.Assets {
		if asset.SidecarPath != "" {
			continue
		}
		for _, candidate := range []string{asset.OriginalPath + ".xmp", strings.TrimSuffix(asset.OriginalPath, filepath.Ext(asset.OriginalPath)) + ".xmp"} {
			var local string
			if strings.HasPrefix(candidate, f.config.ImmichBridgeRoot+string(filepath.Separator)) {
				rel, err := filepath.Rel(f.config.ImmichBridgeRoot, candidate)
				if err != nil {
					continue
				}
				local = filepath.Join(f.config.BridgeRoot, rel)
			} else {
				mapped, err := f.config.SourcePath(candidate)
				if err != nil {
					continue
				}
				local = mapped
			}
			if info, err := os.Lstat(local); err == nil && info.Mode().IsRegular() {
				asset.Sidecar = true
				asset.SidecarPath = candidate
				f.state.Assets[id] = asset
				break
			}
		}
	}
	return f.save()
}
func (f *Client) RefreshMetadata(_ context.Context, _ domain.Member, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail()
}
func (f *Client) FindByPath(_ context.Context, m domain.Member, path string) ([]domain.Asset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return nil, err
	}
	out := []domain.Asset{}
	for _, a := range f.state.Assets {
		if a.OwnerID == m.UserID && a.LibraryID == m.LibraryID && a.OriginalPath == path {
			out = append(out, a)
		}
	}
	return out, nil
}
