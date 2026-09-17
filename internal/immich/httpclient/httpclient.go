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
	"strings"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
)

type Client struct {
	BaseURL  string
	HTTP     *http.Client
	AdminKey string
}

func New(base string) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) request(ctx context.Context, key, method, path string, body any, out any) error {
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
		return domain.Asset{}, e
	}
	out := a.domain()
	var files []struct {
		Type     string `json:"type"`
		IsEdited bool   `json:"isEdited"`
	}
	e = c.request(ctx, m.Key, "GET", "/asset-files?assetId="+url.QueryEscape(id), nil, &files)
	if e != nil {
		return domain.Asset{}, e
	}
	for _, file := range files {
		if file.Type == "sidecar" {
			out.Sidecar = true
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
	x.Stacked = len(a.Stack) > 0 && string(a.Stack) != "null"
	return x
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
	for pages := 0; pages < 10000; pages++ {
		if cursor != "" {
			filter["cursor"] = cursor
		}
		var v struct {
			Assets struct {
				Items      []assetDTO `json:"items"`
				NextCursor *string    `json:"nextCursor"`
			} `json:"assets"`
		}
		if err := c.request(ctx, m.Key, "POST", "/search/metadata", filter, &v); err != nil {
			return nil, err
		}
		for _, a := range v.Assets.Items {
			out = append(out, a.domain())
		}
		if v.Assets.NextCursor == nil || *v.Assets.NextCursor == "" {
			return out, nil
		}
		if *v.Assets.NextCursor == cursor {
			return nil, errors.New("immich search cursor did not advance")
		}
		cursor = *v.Assets.NextCursor
	}
	return nil, errors.New("immich search page limit exceeded")
}
func (c *Client) ListAlbumAssets(ctx context.Context, m domain.Member, id string) ([]domain.Asset, error) {
	return c.search(ctx, m, map[string]any{"albumIds": []string{id}, "size": 1000})
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
