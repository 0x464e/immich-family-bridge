package httpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/domain"
)

func TestPinnedOpenAPISubset(t *testing.T) {
	b, e := os.ReadFile("../../../testdata/openapi/immich-3.2.0.json")
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
	if spec.Info.Version != "3.2.0" {
		t.Fatalf("spec version %s", spec.Info.Version)
	}
	for path, method := range map[string]string{"/users/me": "get", "/assets/{id}": "get", "/asset-files": "get", "/search/metadata": "post", "/albums": "post", "/albums/{id}/assets": "put", "/libraries/{id}/scan": "post", "/shared-links": "post"} {
		if spec.Paths[path][method] == nil {
			t.Fatalf("missing %s %s", method, path)
		}
	}
	for _, field := range []string{"ownerId", "originalPath", "libraryId", "livePhotoVideoId"} {
		if spec.Components.Schemas["AssetResponseDto"].Properties[field] == nil {
			t.Fatalf("missing asset field %s", field)
		}
	}
	for _, field := range []string{"originalPath", "libraryId", "albumIds", "cursor"} {
		if spec.Components.Schemas["MetadataSearchDto"].Properties[field] == nil {
			t.Fatalf("missing search field %s", field)
		}
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
