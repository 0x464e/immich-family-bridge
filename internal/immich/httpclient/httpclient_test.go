package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
)

func TestPinnedOpenAPISubset(t *testing.T) {
	b, e := os.ReadFile("../../../testdata/openapi/immich-3.2.2.json")
	if e != nil {
		t.Fatal(e)
	}
	var spec struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if e := json.Unmarshal(b, &spec); e != nil {
		t.Fatal(e)
	}
	if spec.Info.Version != "3.2.2" {
		t.Fatalf("spec version %s", spec.Info.Version)
	}
	for path, method := range map[string]string{"/users/me": "get", "/assets": "delete", "/assets/{id}": "get", "/asset-files": "get", "/assets/jobs": "post", "/search/metadata": "post", "/albums": "post", "/albums/{id}/assets": "put", "/libraries/{id}/scan": "post", "/jobs/{name}": "put", "/shared-links": "post"} {
		if spec.Paths[path][method] == nil {
			t.Fatalf("missing %s %s", method, path)
		}
	}
	for _, field := range []string{"ownerId", "originalPath", "libraryId", "livePhotoVideoId"} {
		if spec.Components.Schemas["AssetResponseDto"].Properties[field] == nil {
			t.Fatalf("missing asset field %s", field)
		}
	}
	for _, field := range []string{"originalPath", "libraryId", "albumIds", "cursor", "page"} {
		if spec.Components.Schemas["MetadataSearchDto"].Properties[field] == nil {
			t.Fatalf("missing search field %s", field)
		}
	}
	for _, field := range []string{"nextCursor", "nextPage"} {
		if spec.Components.Schemas["SearchAssetResponseDto"].Properties[field] == nil {
			t.Fatalf("missing search response field %s", field)
		}
	}
}

func TestSearchReadsEveryPage(t *testing.T) {
	for _, mode := range []string{"page", "cursor"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				second := (mode == "page" && body["page"] == float64(2)) || (mode == "cursor" && body["cursor"] == "second")
				items := make([]map[string]string, 0, 1000)
				start, end := 0, 1000
				if second {
					start, end = 1000, 1200
				}
				for i := start; i < end; i++ {
					items = append(items, map[string]string{"id": fmt.Sprintf("asset-%04d", i), "ownerId": "owner", "type": "IMAGE"})
				}
				assets := map[string]any{"items": items, "nextCursor": nil, "nextPage": nil}
				if !second {
					if mode == "page" {
						assets["nextPage"] = "2"
					} else {
						assets["nextCursor"] = "second"
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"assets": assets})
			}))
			defer server.Close()
			c := New(server.URL + "/api")
			assets, err := c.ListAlbumAssets(context.Background(), domain.Member{Key: "secret"}, "album")
			if err != nil || len(assets) != 1200 || calls != 2 || assets[1199].ID != "asset-1199" {
				t.Fatalf("pagination: count=%d calls=%d error=%v", len(assets), calls, err)
			}
		})
	}
}

func TestClientSearchAndBulkFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "secret" {
			t.Errorf("missing API key")
		}
		switch r.URL.Path {
		case "/api/search/metadata":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["originalPath"] != "/bridge/a.jpg" {
				t.Errorf("search body %#v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"assets":{"items":[{"id":"a","ownerId":"user","libraryId":"lib","originalPath":"/bridge/a.jpg","type":"IMAGE"}],"nextCursor":null}}`))
		case "/api/albums/album/assets":
			_, _ = w.Write([]byte(`[{"id":"a","success":false,"error":"no_permission"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := New(server.URL + "/api")
	m := domain.Member{ID: "alice", UserID: "user", LibraryID: "lib", Key: "secret"}
	assets, e := c.FindByPath(context.Background(), m, "/bridge/a.jpg")
	if e != nil || len(assets) != 1 || assets[0].ID != "a" {
		t.Fatalf("search: %#v %v", assets, e)
	}
	if e := c.AddAssets(context.Background(), m, "album", []string{"a"}); e == nil {
		t.Fatal("bulk item failure ignored")
	}
}

func TestGetAssetRejectsEditedComponent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/assets/asset":
			_, _ = w.Write([]byte(`{"id":"asset","ownerId":"user","originalPath":"/data/a.jpg","type":"IMAGE"}`))
		case "/api/asset-files":
			_, _ = w.Write([]byte(`[{"type":"preview","isEdited":true}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := New(server.URL + "/api")
	a, err := c.GetAsset(context.Background(), domain.Member{Key: "secret"}, "asset")
	if err != nil || a.Supported() {
		t.Fatalf("edited asset should be unsupported: %+v %v", a, err)
	}
}

func TestGetAssetIncludesSupportedSidecarPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/assets/asset":
			_, _ = w.Write([]byte(`{"id":"asset","ownerId":"user","originalPath":"/data/a.jpg","type":"IMAGE"}`))
		case "/api/asset-files":
			_, _ = w.Write([]byte(`[{"type":"sidecar","path":"/data/a.jpg.xmp","isEdited":false}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := New(server.URL + "/api")
	a, err := c.GetAsset(context.Background(), domain.Member{Key: "secret"}, "asset")
	if err != nil || !a.Supported() || !a.Sidecar || a.SidecarPath != "/data/a.jpg.xmp" {
		t.Fatalf("sidecar asset: %+v %v", a, err)
	}
}

func TestGetAssetConfirmsImmich400AsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/assets/missing":
			http.Error(w, `{"message":"Not found or no asset.read access"}`, http.StatusBadRequest)
		case "/api/search/metadata":
			_, _ = w.Write([]byte(`{"assets":{"items":[],"nextCursor":null}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := New(server.URL + "/api")
	if _, err := c.GetAsset(context.Background(), domain.Member{Key: "member"}, "missing"); !errors.Is(err, immich.ErrNotFound) {
		t.Fatalf("missing asset error: %v", err)
	}
}

func TestSidecarJobsUseDocumentedRequests(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/api/jobs/sidecar":
			if r.Method != http.MethodPut || r.Header.Get("x-api-key") != "admin" || body["command"] != "start" || body["force"] != false {
				t.Errorf("sidecar discovery request: %s %#v", r.Method, body)
			}
		case "/api/assets/jobs":
			ids, _ := body["assetIds"].([]any)
			if r.Method != http.MethodPost || r.Header.Get("x-api-key") != "member" || body["name"] != "refresh-metadata" || len(ids) != 1 || ids[0] != "asset" {
				t.Errorf("metadata refresh request: %s %#v", r.Method, body)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := New(server.URL + "/api")
	c.AdminKey = "admin"
	if err := c.DiscoverSidecars(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshMetadata(context.Background(), domain.Member{Key: "member"}, []string{"asset"}); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestDeleteAssetsUsesForcedMemberRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/assets" || r.Header.Get("x-api-key") != "member" {
			t.Errorf("delete request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			IDs   []string `json:"ids"`
			Force bool     `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !body.Force || len(body.IDs) != 1 || body.IDs[0] != "asset" {
			t.Errorf("delete body: %#v", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c := New(server.URL + "/api")
	if err := c.DeleteAssets(context.Background(), domain.Member{Key: "member"}, []string{"asset"}); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionsReadsCurrentAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/api-keys/me" || r.Header.Get("x-api-key") != "member" {
			t.Errorf("permissions request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"permissions":["asset.read","asset.delete"]}`))
	}))
	defer server.Close()
	c := New(server.URL + "/api")
	permissions, err := c.Permissions(context.Background(), domain.Member{Key: "member"})
	if err != nil || len(permissions) != 2 || permissions[1] != "asset.delete" {
		t.Fatalf("permissions: %#v, %v", permissions, err)
	}
}

func TestReadOnlyClientAllowsSearchButBlocksWrites(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/search/metadata" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"assets":{"items":[],"nextCursor":null}}`))
	}))
	defer server.Close()
	c := New(server.URL + "/api")
	c.ReadOnly = true
	c.AdminKey = "admin-key"
	m := domain.Member{ID: "alice", LibraryID: "library", Key: "member-key"}
	if _, err := c.FindByPath(context.Background(), m, "/bridge/photo.jpg"); err != nil {
		t.Fatal("read-only search:", err)
	}
	for _, call := range []func() error{
		func() error {
			_, err := c.CreateAlbum(context.Background(), m, "id", "name", "description")
			return err
		},
		func() error { return c.UpdateAlbum(context.Background(), m, "album", "name", "description", "") },
		func() error { return c.AddAssets(context.Background(), m, "album", []string{"asset"}) },
		func() error { return c.RemoveAssets(context.Background(), m, "album", []string{"asset"}) },
		func() error { return c.DeleteAssets(context.Background(), m, []string{"asset"}) },
		func() error { return c.ScanLibrary(context.Background(), m) },
		func() error { return c.DiscoverSidecars(context.Background()) },
		func() error { return c.RefreshMetadata(context.Background(), m, []string{"asset"}) },
	} {
		if err := call(); err == nil {
			t.Fatal("read-only client accepted a write")
		}
	}
	if requests != 1 {
		t.Fatalf("write reached Immich HTTP server: %d requests", requests)
	}
}
