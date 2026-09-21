// Package httpclient implements the documented Immich 3.2 API subset.
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
)

type Client struct {
	BaseURL  string
	HTTP     *http.Client
	AdminKey string
	ReadOnly bool
}

func New(base string) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) request(ctx context.Context, key, method, path string, body any, out any) error {
	if c.ReadOnly && method != http.MethodGet && !(method == http.MethodPost && path == "/search/metadata") {
		return errors.New("dry-run mode blocks Immich API write")
	}
	var r io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		r = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if e != nil {
		return e
	}
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, e := client.Do(req)
	if e != nil {
		return fmt.Errorf("immich API: %w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s %s", immich.ErrNotFound, method, path)
		}
		return fmt.Errorf("immich API %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Major int `json:"major"`
		Minor int `json:"minor"`
		Patch int `json:"patch"`
	}
	e := c.request(ctx, "", "GET", "/server/version", nil, &v)
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch), e
}
func (c *Client) Me(ctx context.Context, m domain.Member) (string, error) {
	var v struct {
		ID string `json:"id"`
	}
	e := c.request(ctx, m.Key, "GET", "/users/me", nil, &v)
	return v.ID, e
}

// SessionUser returns the Immich user identified by a browser session cookie.
// It deliberately does not use an API key: this is used only by the narrowly
// scoped link resolver, which replays the browser's existing Immich session.
func (c *Client) SessionUser(ctx context.Context, cookie string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/users/me", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Cookie", cookie)
	request.Header.Set("Accept", "application/json")
	client := http.DefaultClient
	if c.HTTP != nil {
		copy := *c.HTTP
		client = &copy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("immich session identity: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("immich session identity: HTTP %d", response.StatusCode)
	}
	var user struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&user); err != nil {
		return "", fmt.Errorf("immich session identity: %w", err)
	}
	if user.ID == "" {
		return "", errors.New("immich session identity response omitted user id")
	}
	return user.ID, nil
}
func (c *Client) Permissions(ctx context.Context, m domain.Member) ([]string, error) {
	var v struct {
		Permissions []string `json:"permissions"`
	}
	err := c.request(ctx, m.Key, "GET", "/api-keys/me", nil, &v)
	return v.Permissions, err
}
func (c *Client) GetLibrary(ctx context.Context, m domain.Member) (domain.Library, error) {
	var library domain.Library
	if c.AdminKey == "" {
		return library, errors.New("admin API key required to inspect external library")
	}
	err := c.request(ctx, c.AdminKey, "GET", "/libraries/"+url.PathEscape(m.LibraryID), nil, &library)
	return library, err
}
func (c *Client) GetAsset(ctx context.Context, m domain.Member, id string) (domain.Asset, error) {
	var a assetDTO
	e := c.request(ctx, m.Key, "GET", "/assets/"+url.PathEscape(id), nil, &a)
	if e != nil {
		// Immich 3.2 returns HTTP 400, rather than 404, for a missing or
		// inaccessible asset. Confirm absence with the documented metadata
		// search so a changed permission is not mistaken for deletion.
		if strings.Contains(e.Error(), "HTTP 400") {
			found, searchErr := c.search(ctx, m, map[string]any{"id": id, "size": 1, "withDeleted": true})
			if searchErr == nil && len(found) == 0 {
				return domain.Asset{}, fmt.Errorf("%w: GET /assets/%s", immich.ErrNotFound, id)
			}
		}
		return domain.Asset{}, e
	}
	out := a.domain()
	var files []struct {
		Type     string `json:"type"`
		IsEdited bool   `json:"isEdited"`
		Path     string `json:"path"`
	}
	e = c.request(ctx, m.Key, "GET", "/asset-files?assetId="+url.QueryEscape(id), nil, &files)
	if e != nil {
		return domain.Asset{}, e
	}
	for _, file := range files {
		if file.Type == "sidecar" {
			out.Sidecar = true
			out.SidecarPath = file.Path
		}
		if file.IsEdited {
			out.Edited = true
		}
	}
	return out, nil
}

type assetDTO struct {
	ID               string          `json:"id"`
	OwnerID          string          `json:"ownerId"`
	LibraryID        *string         `json:"libraryId"`
	OriginalPath     string          `json:"originalPath"`
	OriginalFileName string          `json:"originalFileName"`
	Type             string          `json:"type"`
	LivePhotoVideoID *string         `json:"livePhotoVideoId"`
	Stack            json.RawMessage `json:"stack"`
	IsEdited         bool            `json:"isEdited"`
}

func (a assetDTO) domain() domain.Asset {
	x := domain.Asset{ID: a.ID, OwnerID: a.OwnerID, OriginalPath: a.OriginalPath, OriginalFileName: a.OriginalFileName, Type: a.Type, Edited: a.IsEdited}
	if a.LibraryID != nil {
		x.LibraryID = *a.LibraryID
	}
	if a.LivePhotoVideoID != nil {
		x.LivePhotoVideoID = *a.LivePhotoVideoID
	}
	if len(a.Stack) > 0 && string(a.Stack) != "null" {
		var stack struct {
			ID             string `json:"id"`
			PrimaryAssetID string `json:"primaryAssetId"`
		}
		if json.Unmarshal(a.Stack, &stack) == nil {
			x.StackID, x.StackPrimaryID = stack.ID, stack.PrimaryAssetID
		}
	}
	return x
}

type stackDTO struct {
	ID             string     `json:"id"`
	OwnerID        string     `json:"ownerId"`
	PrimaryAssetID string     `json:"primaryAssetId"`
	Assets         []assetDTO `json:"assets"`
}

func (s stackDTO) domain() domain.Stack {
	out := domain.Stack{ID: s.ID, OwnerID: s.OwnerID, PrimaryAssetID: s.PrimaryAssetID, Assets: make([]domain.Asset, 0, len(s.Assets))}
	for _, asset := range s.Assets {
		out.Assets = append(out.Assets, asset.domain())
	}
	return out
}

func (c *Client) GetStack(ctx context.Context, m domain.Member, id string) (domain.Stack, error) {
	var stack stackDTO
	err := c.request(ctx, m.Key, "GET", "/stacks/"+url.PathEscape(id), nil, &stack)
	return stack.domain(), err
}

func (c *Client) ListStacks(ctx context.Context, m domain.Member) ([]domain.Stack, error) {
	var stacks []stackDTO
	if err := c.request(ctx, m.Key, "GET", "/stacks", nil, &stacks); err != nil {
		return nil, err
	}
	out := make([]domain.Stack, 0, len(stacks))
	for _, stack := range stacks {
		out = append(out, stack.domain())
	}
	return out, nil
}

type albumDTO struct {
	ID          string  `json:"id"`
	AlbumName   string  `json:"albumName"`
	Description string  `json:"description"`
	CoverID     *string `json:"albumThumbnailAssetId"`
	Users       []struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		ID string `json:"id"`
	} `json:"albumUsers"`
}

func (a albumDTO) domain() domain.Album {
	x := domain.Album{ID: a.ID, Name: a.AlbumName, Description: a.Description}
	if a.CoverID != nil {
		x.CoverID = *a.CoverID
	}
	if len(a.Users) > 0 {
		x.OwnerID = a.Users[0].User.ID
		if x.OwnerID == "" {
			x.OwnerID = a.Users[0].ID
		}
	}
	return x
}
func (c *Client) GetAlbum(ctx context.Context, m domain.Member, id string) (domain.Album, error) {
	var a albumDTO
	e := c.request(ctx, m.Key, "GET", "/albums/"+url.PathEscape(id), nil, &a)
	return a.domain(), e
}
func (c *Client) ListAlbums(ctx context.Context, m domain.Member) ([]domain.Album, error) {
	var in []albumDTO
	e := c.request(ctx, m.Key, "GET", "/albums?isOwned=true", nil, &in)
	out := make([]domain.Album, 0, len(in))
	for _, a := range in {
		out = append(out, a.domain())
	}
	return out, e
}
func (c *Client) search(ctx context.Context, m domain.Member, filter map[string]any) ([]domain.Asset, error) {
	out := []domain.Asset{}
	cursor := ""
	page := 1
	seen := map[string]bool{}
	for pages := 0; pages < 10000; pages++ {
		if cursor != "" {
			filter["cursor"] = cursor
			delete(filter, "page")
		} else {
			filter["page"] = page
		}
		var v struct {
			Assets struct {
				Items      []assetDTO `json:"items"`
				NextCursor *string    `json:"nextCursor"`
				NextPage   *string    `json:"nextPage"`
			} `json:"assets"`
		}
		if err := c.request(ctx, m.Key, "POST", "/search/metadata", filter, &v); err != nil {
			return nil, err
		}
		for _, a := range v.Assets.Items {
			if seen[a.ID] {
				return nil, fmt.Errorf("immich search returned asset %s on multiple pages", a.ID)
			}
			seen[a.ID] = true
			out = append(out, a.domain())
		}
		if v.Assets.NextCursor != nil && *v.Assets.NextCursor != "" {
			if *v.Assets.NextCursor == cursor {
				return nil, errors.New("immich search cursor did not advance")
			}
			cursor = *v.Assets.NextCursor
			continue
		}
		if v.Assets.NextPage != nil && *v.Assets.NextPage != "" {
			next, err := strconv.Atoi(*v.Assets.NextPage)
			if err != nil || next <= page {
				return nil, fmt.Errorf("immich search page did not advance: %q", *v.Assets.NextPage)
			}
			page = next
			cursor = ""
			continue
		}
		return out, nil
	}
	return nil, errors.New("immich search page limit exceeded")
}
func (c *Client) ListAlbumAssets(ctx context.Context, m domain.Member, id string) ([]domain.Asset, error) {
	// Immich otherwise omits children of a stack from search results. Album
	// membership must observe every member so a source stack cannot look like
	// an unshare of its non-primary assets.
	return c.search(ctx, m, map[string]any{"albumIds": []string{id}, "size": 1000, "withStacked": true})
}
func (c *Client) CreateAlbum(ctx context.Context, m domain.Member, logicalID, name, desc string) (domain.Album, error) {
	var a albumDTO
	e := c.request(ctx, m.Key, "POST", "/albums", map[string]any{"albumName": name, "description": desc}, &a)
	return a.domain(), e
}
func (c *Client) UpdateAlbum(ctx context.Context, m domain.Member, id, name, desc, cover string) error {
	body := map[string]any{"albumName": name, "description": desc}
	if cover != "" {
		body["albumThumbnailAssetId"] = cover
	}
	return c.request(ctx, m.Key, "PATCH", "/albums/"+url.PathEscape(id), body, nil)
}
func (c *Client) AddAssets(ctx context.Context, m domain.Member, id string, assets []string) error {
	return c.changeAssets(ctx, m, id, assets, "PUT")
}
func (c *Client) RemoveAssets(ctx context.Context, m domain.Member, id string, assets []string) error {
	return c.changeAssets(ctx, m, id, assets, "DELETE")
}
func (c *Client) DeleteAssets(ctx context.Context, m domain.Member, assets []string) error {
	return c.request(ctx, m.Key, "DELETE", "/assets", map[string]any{"ids": assets, "force": true}, nil)
}

func (c *Client) CreateStack(ctx context.Context, m domain.Member, assets []string) (domain.Stack, error) {
	var stack stackDTO
	err := c.request(ctx, m.Key, "POST", "/stacks", map[string]any{"assetIds": assets}, &stack)
	return stack.domain(), err
}

func (c *Client) DeleteStack(ctx context.Context, m domain.Member, id string) error {
	return c.request(ctx, m.Key, "DELETE", "/stacks/"+url.PathEscape(id), nil, nil)
}
func (c *Client) changeAssets(ctx context.Context, m domain.Member, id string, assets []string, method string) error {
	var result []struct {
		ID      string `json:"id"`
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := c.request(ctx, m.Key, method, "/albums/"+url.PathEscape(id)+"/assets", map[string]any{"ids": assets}, &result); err != nil {
		return err
	}
	if len(result) != len(assets) {
		return fmt.Errorf("Immich returned %d results for %d album assets", len(result), len(assets))
	}
	for _, item := range result {
		if !item.Success {
			return fmt.Errorf("Immich album asset %s failed: %s", item.ID, item.Error)
		}
	}
	return nil
}
func (c *Client) ScanLibrary(ctx context.Context, m domain.Member) error {
	key := c.AdminKey
	if key == "" {
		return errors.New("admin API key required to scan an external library")
	}
	return c.request(ctx, key, "POST", "/libraries/"+url.PathEscape(m.LibraryID)+"/scan", nil, nil)
}
func (c *Client) DiscoverSidecars(ctx context.Context) error {
	if c.AdminKey == "" {
		return errors.New("admin API key required to discover sidecars")
	}
	return c.request(ctx, c.AdminKey, http.MethodPut, "/jobs/sidecar", map[string]any{"command": "start", "force": false}, nil)
}
func (c *Client) RefreshMetadata(ctx context.Context, m domain.Member, assetIDs []string) error {
	if len(assetIDs) == 0 {
		return nil
	}
	return c.request(ctx, m.Key, http.MethodPost, "/assets/jobs", map[string]any{"name": "refresh-metadata", "assetIds": assetIDs}, nil)
}
func (c *Client) FindByPath(ctx context.Context, m domain.Member, path string) ([]domain.Asset, error) {
	found, e := c.search(ctx, m, map[string]any{"libraryId": m.LibraryID, "originalPath": path, "size": 100})
	if e != nil {
		return nil, e
	}
	out := []domain.Asset{}
	for _, a := range found {
		if a.OwnerID == m.UserID && a.LibraryID == m.LibraryID && a.OriginalPath == path {
			out = append(out, a)
		}
	}
	return out, nil
}
