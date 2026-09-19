package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/config"
	"github.com/0x464e/immich-family-bridge/internal/domain"
	fake "github.com/0x464e/immich-family-bridge/internal/immich/testfake"
	"github.com/0x464e/immich-family-bridge/internal/reconcile"
	"github.com/0x464e/immich-family-bridge/internal/store"
)

const (
	aliceAsset = "11111111-1111-1111-1111-111111111111"
	bobAsset   = "22222222-2222-2222-2222-222222222222"
	aliceAlbum = "33333333-3333-3333-3333-333333333333"
	bobAlbum   = "44444444-4444-4444-4444-444444444444"
)

type sessionStub struct {
	userID string
	err    error
	cookie string
}

func (s *sessionStub) SessionUser(_ context.Context, cookie string) (string, error) {
	s.cookie = cookie
	return s.userID, s.err
}

func TestInternalAPIRequiresToken(t *testing.T) {
	root := t.TempDir()
	c := config.Config{FamilyID: "family", SourceRoot: filepath.Join(root, "source"), BridgeRoot: filepath.Join(root, "bridge"), ImmichBridgeRoot: "/bridge", Database: filepath.Join(root, "state", "db.sqlite"), APIToken: "secret", Members: []domain.Member{{ID: "a", UserID: "u-a", LibraryID: "l-a"}, {ID: "b", UserID: "u-b", LibraryID: "l-b"}}}
	db, e := store.Open(c.Database)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e := db.Init(c.FamilyID, c.Members); e != nil {
		t.Fatal(e)
	}
	api, e := fake.New(c, filepath.Join(root, "state", "fake.json"), fake.Seed{})
	if e != nil {
		t.Fatal(e)
	}
	r := reconcile.New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := (&Server{C: c, DB: db, R: r}).Handler()
	for _, tc := range []struct {
		path, token string
		want        int
	}{{"/healthz", "", 200}, {"/api/members", "", 401}, {"/api/members", "wrong", 401}, {"/api/members", "secret", 200}} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s %q: got %d want %d", tc.path, tc.token, rec.Code, tc.want)
		}
	}
}

func TestDryRunModeReportsAndBlocksActiveEndpoint(t *testing.T) {
	root := t.TempDir()
	c := config.Config{FamilyID: "family", DryRun: true, SourceRoot: filepath.Join(root, "source"), BridgeRoot: filepath.Join(root, "bridge"), ImmichBridgeRoot: "/bridge", Database: filepath.Join(root, "state", "db.sqlite"), APIToken: "secret", Members: []domain.Member{{ID: "a", UserID: "u-a", LibraryID: "l-a"}, {ID: "b", UserID: "u-b", LibraryID: "l-b"}}}
	db, err := store.Open(c.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Init(c.FamilyID, c.Members); err != nil {
		t.Fatal(err)
	}
	api, err := fake.New(c, filepath.Join(root, "state", "fake.json"), fake.Seed{})
	if err != nil {
		t.Fatal(err)
	}
	r := reconcile.New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := (&Server{C: c, DB: db, R: r}).Handler()
	for _, tc := range []struct {
		method, path string
		want         int
		body         string
	}{{"GET", "/api/status", 200, "dry_run"}, {"POST", "/api/reconcile", 409, "dry-run mode enabled"}, {"POST", "/api/reconcile/dry-run", 404, "404 page not found"}} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.body) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestForwardAuthRedirectsOnlyReadyActiveReplica(t *testing.T) {
	s, db := resolverTestServer(t, "u-bob", nil)
	req := httptest.NewRequest(http.MethodGet, "/forward-auth", nil)
	req.SetBasicAuth("familybridge", "forward-secret")
	req.Header.Set("Cookie", "immich_access_token=session")
	req.Header.Set("X-Forwarded-Uri", "/photos/"+aliceAsset+"?from=share")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/photos/"+bobAsset+"?from=share" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("resolver response: %d %#v", rec.Code, rec.Header())
	}
	if s.Session.(*sessionStub).cookie != "immich_access_token=session" {
		t.Fatal("browser cookie not passed to session identity client")
	}
	if _, err := db.DB.Exec(`DELETE FROM sharing_sources`); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unshared asset response = %d", rec.Code)
	}
}

func TestForwardAuthRedirectsMirrorAlbum(t *testing.T) {
	s, _ := resolverTestServer(t, "u-bob", nil)
	req := httptest.NewRequest(http.MethodGet, "/forward-auth", nil)
	req.SetBasicAuth("familybridge", "forward-secret")
	req.Header.Set("Cookie", "immich_access_token=session")
	req.Header.Set("X-Forwarded-Uri", "/albums/"+aliceAlbum+"?from=share")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/albums/"+bobAlbum+"?from=share" {
		t.Fatalf("resolver response: %d %#v", rec.Code, rec.Header())
	}
	req.Header.Set("X-Forwarded-Uri", "/albums/11111111-1111-1111-1111-111111111111")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unregistered album response = %d", rec.Code)
	}
}

func TestForwardAuthFailsOpenAndValidatesInput(t *testing.T) {
	for _, tc := range []struct {
		name, uri, user, cookie string
		err                     error
		basic                   bool
		want                    int
	}{
		{name: "missing credential", uri: "/photos/" + aliceAsset, user: "u-bob", cookie: "session", want: http.StatusUnauthorized},
		{name: "missing cookie", uri: "/photos/" + aliceAsset, user: "u-bob", basic: true, want: http.StatusNoContent},
		{name: "identity failure", uri: "/photos/" + aliceAsset, cookie: "session", err: errors.New("unavailable"), basic: true, want: http.StatusNoContent},
		{name: "non member", uri: "/photos/" + aliceAsset, user: "not-a-member", cookie: "session", basic: true, want: http.StatusNoContent},
		{name: "same replica", uri: "/photos/" + aliceAsset, user: "u-alice", cookie: "session", basic: true, want: http.StatusNoContent},
		{name: "non uuid path", uri: "/photos/not-an-asset", user: "u-bob", cookie: "session", basic: true, want: http.StatusNoContent},
		{name: "nested path", uri: "/photos/" + aliceAsset + "/extra", user: "u-bob", cookie: "session", basic: true, want: http.StatusNoContent},
		{name: "invalid album path", uri: "/albums/not-an-album", user: "u-bob", cookie: "session", basic: true, want: http.StatusNoContent},
		{name: "relative path", uri: "photos/" + aliceAsset, user: "u-bob", cookie: "session", basic: true, want: http.StatusNoContent},
		{name: "external uri", uri: "https://example.test/photos/" + aliceAsset, user: "u-bob", cookie: "session", basic: true, want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := resolverTestServer(t, tc.user, tc.err)
			req := httptest.NewRequest(http.MethodGet, "/forward-auth", nil)
			if tc.basic {
				req.SetBasicAuth("familybridge", "forward-secret")
			}
			req.Header.Set("Cookie", tc.cookie)
			req.Header.Set("X-Forwarded-Uri", tc.uri)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func resolverTestServer(t *testing.T, userID string, sessionErr error) (*Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "bridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := config.Config{FamilyID: "family", ForwardAuthToken: "forward-secret", Members: []domain.Member{{ID: "alice", UserID: "u-alice", LibraryID: "l-alice"}, {ID: "bob", UserID: "u-bob", LibraryID: "l-bob"}}}
	if err := db.Init(c.FamilyID, c.Members); err != nil {
		t.Fatal(err)
	}
	logical, err := db.EnsureOrigin(c.FamilyID, "alice", domain.Asset{ID: aliceAsset, OwnerID: "u-alice", OriginalPath: "/fixture/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertReplica(domain.Replica{LogicalID: logical, MemberID: "bob", AssetID: bobAsset, Role: "external_replica", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO sharing_sources(logical_asset_id,source_kind,source_id) VALUES(?,'album','together')`, logical); err != nil {
		t.Fatal(err)
	}
	if err := db.AddAlbum(c.FamilyID, domain.LogicalAlbum{ID: "together", Name: "Together"}, map[string]string{"alice": aliceAlbum, "bob": bobAlbum}); err != nil {
		t.Fatal(err)
	}
	return &Server{C: c, DB: db, Session: &sessionStub{userID: userID, err: sessionErr}}, db
}
