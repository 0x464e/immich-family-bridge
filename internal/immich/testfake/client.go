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
}

// Seed is used only by unit tests. The running service never constructs this client.
type Seed struct {
	Assets []SeedAsset
	Albums []SeedAlbum
}
type SeedAsset struct {
	ID, Member, Path, Type string
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
	f := &Client{config: c, statePath: statePath, state: State{Assets: map[string]domain.Asset{}, Albums: map[string]domain.Album{}, Membership: map[string]map[string]bool{}}}
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
		f.state.Assets[a.ID] = domain.Asset{ID: a.ID, OwnerID: m.UserID, OriginalPath: a.Path, OriginalFileName: filepath.Base(a.Path), Type: typ}
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
		f.state.Assets[id] = domain.Asset{ID: id, OwnerID: m.UserID, LibraryID: m.LibraryID, OriginalPath: immichPath, OriginalFileName: filepath.Base(path), Type: typ}
		return nil
	})
	return f.save()
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
