package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/config"
	"github.com/0x464e/immich-family-bridge/internal/reconcile"
	"github.com/0x464e/immich-family-bridge/internal/store"
)

type Server struct {
	C       config.Config
	DB      *store.Store
	R       *reconcile.Reconciler
	Session SessionIdentity
}

// SessionIdentity resolves an Immich browser session without interpreting its
// cookie. The concrete HTTP adapter replays the cookie to Immich /users/me.
type SessionIdentity interface {
	SessionUser(context.Context, string) (string, error)
}

var immichAssetID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const resolverTimeout = 2 * time.Second

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /forward-auth", s.forwardAuth)
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.DB.DB.PingContext(ctx); err != nil {
			write(w, 503, map[string]string{"error": "database unavailable"})
			return
		}
		if err := s.R.Check(ctx); err != nil {
			write(w, 503, map[string]string{"error": "Immich backend unavailable or member mismatch"})
			return
		}
		write(w, 200, map[string]string{"status": "ready"})
	})
	api := http.NewServeMux()
	api.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		mode := "active"
		if s.C.DryRun {
			mode = "dry_run"
		}
		write(w, 200, map[string]string{"mode": mode})
	})
	api.HandleFunc("GET /api/members", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]string, 0, len(s.C.Members))
		for _, m := range s.C.Members {
			out = append(out, map[string]string{"id": m.ID, "userId": m.UserID, "libraryId": m.LibraryID})
		}
		write(w, 200, out)
	})
	api.HandleFunc("GET /api/albums", func(w http.ResponseWriter, r *http.Request) {
		a, e := s.DB.Albums()
		if e != nil {
			write(w, 500, map[string]string{"error": e.Error()})
			return
		}
		write(w, 200, a)
	})
	api.HandleFunc("GET /api/replicas", func(w http.ResponseWriter, r *http.Request) {
		a, e := s.DB.Replicas()
		if e != nil {
			write(w, 500, map[string]string{"error": e.Error()})
			return
		}
		write(w, 200, a)
	})
	api.HandleFunc("GET /api/assets/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		asset, e := s.DB.LogicalAsset(id)
		if e != nil {
			write(w, 404, map[string]string{"error": "logical asset not found"})
			return
		}
		replicas, e := s.DB.ReplicasFor(id)
		if e != nil {
			write(w, 500, map[string]string{"error": e.Error()})
			return
		}
		components := map[string]any{}
		for _, replica := range replicas {
			files, err := s.DB.ReplicaFiles(id, replica.MemberID)
			if err != nil {
				write(w, 500, map[string]string{"error": err.Error()})
				return
			}
			components[replica.MemberID] = files
		}
		write(w, 200, map[string]any{"asset": asset, "replicas": replicas, "components": components})
	})
	api.HandleFunc("GET /api/filesystem", func(w http.ResponseWriter, r *http.Request) {
		reps, e := s.DB.Replicas()
		if e != nil {
			write(w, 500, map[string]string{"error": e.Error()})
			return
		}
		out := []map[string]any{}
		for _, rep := range reps {
			if rep.Role != "external_replica" {
				continue
			}
			src, found, e := s.DB.Replica(rep.LogicalID, s.originMember(rep.LogicalID))
			if e != nil || !found {
				continue
			}
			sourceInfo, e1 := os.Stat(src.Path)
			destInfo, e2 := os.Stat(rep.Path)
			item := map[string]any{"logicalAssetId": rep.LogicalID, "memberId": rep.MemberID, "path": rep.Path, "exists": e2 == nil, "sameInode": e1 == nil && e2 == nil && os.SameFile(sourceInfo, destInfo)}
			if e2 == nil {
				for k, v := range inodeDetails(destInfo) {
					item[k] = v
				}
			}
			out = append(out, item)
		}
		write(w, 200, out)
	})
	api.HandleFunc("POST /api/albums", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MemberID string            `json:"memberId"`
			AlbumID  string            `json:"albumId"`
			Replicas map[string]string `json:"replicas"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || body.MemberID == "" || body.AlbumID == "" {
			write(w, 400, map[string]string{"error": "memberId and albumId required"})
			return
		}
		id, e := s.R.RegisterWithReplicas(r.Context(), body.MemberID, body.AlbumID, body.Replicas)
		if e != nil {
			write(w, 409, map[string]string{"error": e.Error()})
			return
		}
		write(w, 201, map[string]string{"id": id})
	})
	api.HandleFunc("PATCH /api/albums/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name                string `json:"name"`
			Description         string `json:"description"`
			CoverLogicalAssetID string `json:"coverLogicalAssetId"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
			write(w, 400, map[string]string{"error": "invalid JSON"})
			return
		}
		if e := s.R.UpdateAlbum(r.PathValue("id"), body.Name, body.Description, body.CoverLogicalAssetID); e != nil {
			write(w, 400, map[string]string{"error": e.Error()})
			return
		}
		write(w, 200, map[string]string{"status": "ok"})
	})
	api.HandleFunc("POST /api/reconcile", func(w http.ResponseWriter, r *http.Request) {
		if s.C.DryRun {
			write(w, 409, map[string]string{"error": reconcile.ErrDryRunMode.Error()})
			return
		}
		if e := s.R.Run(r.Context()); e != nil {
			write(w, 503, map[string]string{"error": e.Error()})
			return
		}
		write(w, 200, map[string]string{"status": "ok"})
	})
	mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.C.APIToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.C.APIToken)) != 1 {
			write(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		api.ServeHTTP(w, r)
	}))
	return mux
}

// forwardAuth is called only by Traefik's narrowly matched ForwardAuth
// middleware. It fails open for every resolver failure so ordinary Immich
// handling (including its login/deep-link flow) remains authoritative.
func (s *Server) forwardAuth(w http.ResponseWriter, r *http.Request) {
	if !s.validForwardAuthCredential(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="immich-family-bridge"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	resource, requestedID, rawQuery, ok := forwardedImmichLink(r.Header.Get("X-Forwarded-Uri"))
	if !ok || s.Session == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cookie := r.Header.Get("Cookie")
	if cookie == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), resolverTimeout)
	defer cancel()
	userID, err := s.Session.SessionUser(ctx, cookie)
	if err != nil || !s.isMemberUser(userID) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var targetID string
	var found bool
	if resource == "photos" {
		targetID, found, err = s.DB.ResolveActiveReplica(ctx, requestedID, userID)
	} else {
		targetID, found, err = s.DB.ResolveMirrorAlbum(ctx, requestedID, userID)
	}
	if err != nil || !found || targetID == requestedID || !immichAssetID.MatchString(targetID) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", "/"+resource+"/"+targetID+rawQuery)
	w.WriteHeader(http.StatusTemporaryRedirect)
}

func (s *Server) validForwardAuthCredential(r *http.Request) bool {
	username, token, ok := r.BasicAuth()
	return ok && username == "familybridge" && s.C.ForwardAuthToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.C.ForwardAuthToken)) == 1
}

func (s *Server) isMemberUser(userID string) bool {
	for _, member := range s.C.Members {
		if member.UserID == userID {
			return true
		}
	}
	return false
}

func forwardedImmichLink(rawURI string) (resource, id, rawQuery string, ok bool) {
	u, err := url.ParseRequestURI(rawURI)
	if err != nil || u.IsAbs() || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, "/") {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 || (parts[0] != "photos" && parts[0] != "albums") || !immichAssetID.MatchString(parts[1]) {
		return "", "", "", false
	}
	resource, id = parts[0], parts[1]
	if u.RawQuery == "" {
		return resource, id, "", true
	}
	return resource, id, "?" + u.RawQuery, true
}

func (s *Server) originMember(id string) string {
	a, e := s.DB.LogicalAsset(id)
	if e != nil {
		return ""
	}
	return a.OriginMember
}
