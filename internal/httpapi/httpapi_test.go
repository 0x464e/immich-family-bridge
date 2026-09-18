package httpapi

import (
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
	}{{"GET", "/api/status", 200, "dry_run"}, {"POST", "/api/reconcile", 409, "dry-run mode enabled"}} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.body) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
