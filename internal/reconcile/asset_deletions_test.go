package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
	"github.com/0x464e/immich-family-bridge/internal/store"
)

func TestOwnerDeletionPropagatesAcrossAlbumsAndRestart(t *testing.T) {
	for _, trash := range []bool{false, true} {
		for _, cleanup := range []bool{false, true} {
			t.Run(fmt.Sprintf("trash=%t/cleanup=%t", trash, cleanup), func(t *testing.T) {
				c, db, api, r := setup(t)
				ctx := context.Background()
				c.RemoveUnshared = cleanup
				r.C = c
				sidecar := filepath.Join(c.SourceRoot, "a.jpg.xmp")
				if err := os.WriteFile(sidecar, []byte("<xmp/>"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := api.SetSidecar("a1", sidecar); err != nil {
					t.Fatal(err)
				}
				for _, aid := range []string{"trip", "best"} {
					if _, err := r.Register(ctx, "alice", aid); err != nil {
						t.Fatal(err)
					}
				}
				if err := r.Run(ctx); err != nil {
					t.Fatal(err)
				}
				lid, _, _ := db.FindReplicaAsset("alice", "a1")
				bob, _, _ := db.Replica(lid, "bob")
				if trash {
					if err := api.TrashAsset("a1"); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := api.DeleteAsset("a1"); err != nil {
						t.Fatal(err)
					}
					// A permanently deleted managed original and its sidecar no longer exist.
					for _, p := range []string{filepath.Join(c.SourceRoot, "a.jpg"), sidecar} {
						if err := os.Remove(p); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := r.Run(ctx); err != nil {
					t.Fatal(err)
				}
				if refs, err := db.SourceCount(lid); err != nil || refs != 0 {
					t.Fatalf("references=%d err=%v", refs, err)
				}
				origin, _, _ := db.Replica(lid, "alice")
				if origin.State != "owner_deleted" {
					t.Fatalf("origin=%+v", origin)
				}
				pending, _, _ := db.Replica(lid, "bob")
				if pending.State != "pending_removal" {
					t.Fatalf("pending=%+v", pending)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := store.Open(c.Database)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				r = New(c, reopened, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
				for i := 0; i < 3; i++ {
					if err := r.Run(ctx); err != nil {
						t.Fatal(err)
					}
				}
				for _, m := range c.Members[1:] {
					if rep, found, err := reopened.Replica(lid, m.ID); err != nil || found {
						t.Fatalf("recipient survives: %+v %v", rep, err)
					}
				}
				for _, p := range []string{bob.Path, bob.Path + ".xmp"} {
					if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("file survives: %s %v", p, err)
					}
				}
				if trash {
					a, err := api.GetAsset(ctx, c.Members[0], "a1")
					if err != nil || !a.IsTrashed {
						t.Fatalf("owner trash altered: %+v %v", a, err)
					}
					if _, err := os.Stat(sidecar); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestRecipientDeletionRestoresWithoutUnsharing(t *testing.T) {
	for _, trash := range []bool{false, true} {
		t.Run(fmt.Sprint(trash), func(t *testing.T) {
			c, db, api, r := setup(t)
			ctx := context.Background()
			r.C.RemoveUnshared = true
			if _, err := r.Register(ctx, "alice", "trip"); err != nil {
				t.Fatal(err)
			}
			if err := r.Run(ctx); err != nil {
				t.Fatal(err)
			}
			lid, _, _ := db.FindReplicaAsset("alice", "a1")
			old, _, _ := db.Replica(lid, "bob")
			if trash {
				if err := api.TrashAsset(old.AssetID); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := api.DeleteAsset(old.AssetID); err != nil {
					t.Fatal(err)
				}
			}
			r = New(r.C, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
			r.scanInterval = 0
			// Force the repair path instead of relying on the bounded ready audit.
			r.batchCursor = map[string]string{}
			for i := 0; i < 3; i++ {
				if err := r.Run(ctx); err != nil {
					t.Fatal(err)
				}
			}
			rep, _, _ := db.Replica(lid, "bob")
			a, err := api.GetAsset(ctx, c.Members[1], rep.AssetID)
			if err != nil || a.IsTrashed || rep.State != "ready" {
				t.Fatalf("not restored: %+v %+v %v", rep, a, err)
			}
			if trash && rep.AssetID != old.AssetID {
				t.Fatal("trash restoration should preserve asset ID")
			}
			albums, _ := db.Albums()
			for _, album := range albums {
				reps, _ := db.AlbumReplicas(album.ID)
				assets, err := api.ListAlbumAssets(ctx, c.Members[1], reps["bob"])
				if err != nil || len(assets) != 1 || assets[0].ID != rep.AssetID {
					t.Fatalf("membership not restored: %+v %v", assets, err)
				}
			}
			if _, err := api.GetAsset(ctx, c.Members[0], "a1"); err != nil {
				t.Fatal("origin changed", err)
			}
		})
	}
}

type assetReadFailure struct {
	immich.Client
	id string
}

func (c assetReadFailure) GetAsset(ctx context.Context, m domain.Member, id string) (domain.Asset, error) {
	if id == c.id {
		return domain.Asset{}, errors.New("HTTP 403")
	}
	return c.Client.GetAsset(ctx, m, id)
}

func TestOwnerDeletionReadFailureAndDryRunDoNotWrite(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	lid, _, _ := db.FindReplicaAsset("alice", "a1")
	if err := api.DeleteAsset("a1"); err != nil {
		t.Fatal(err)
	}
	r.API = assetReadFailure{Client: api, id: "a1"}
	if err := r.Run(ctx); err == nil {
		t.Fatal("read failure accepted")
	}
	if n, _ := db.SourceCount(lid); n == 0 {
		t.Fatal("read failure revoked sharing")
	}
	r.API = api
	r.C.DryRun = true
	actions, err := r.DryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range actions {
		if a.Kind == "remove_sharing_reference" && a.LogicalAssetID == lid {
			found = true
		}
	}
	if !found {
		t.Fatalf("dry run omitted owner deletion: %+v", actions)
	}
	if n, _ := db.SourceCount(lid); n == 0 {
		t.Fatal("dry run revoked sharing")
	}
	origin, _, _ := db.Replica(lid, c.Members[0].ID)
	if origin.State == "owner_deleted" {
		t.Fatal("dry run persisted owner deletion")
	}
}

func TestOwnerRestoreCanDeliberatelyReshare(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	lid, _, _ := db.FindReplicaAsset("alice", "a1")
	if err := api.TrashAsset("a1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.RestoreAssets(ctx, c.Members[0], []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	if err := api.AddAssets(ctx, c.Members[0], "trip", []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	r.scanInterval = 0
	for i := 0; i < 2; i++ {
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range c.Members {
		rep, found, err := db.Replica(lid, m.ID)
		if err != nil || !found || rep.State != "ready" {
			t.Fatalf("not reshared: %+v %v", rep, err)
		}
	}
}

type restoreFailure struct{ immich.Client }

func (c restoreFailure) RestoreAssets(context.Context, domain.Member, []string) error {
	return errors.New("temporary restore failure")
}

func TestRecipientRestoreFailureRetriesAfterRestart(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	lid, _, _ := db.FindReplicaAsset("alice", "a1")
	rep, _, _ := db.Replica(lid, "bob")
	if err := api.TrashAsset(rep.AssetID); err != nil {
		t.Fatal(err)
	}
	r.API = restoreFailure{Client: api}
	// Preparation failures stay retryable without changing the desired set.
	_ = r.Run(ctx)
	if refs, err := db.SourceCount(lid); err != nil || refs == 0 {
		t.Fatalf("restore failure revoked sharing: %d %v", refs, err)
	}
	pending, _, _ := db.Replica(lid, "bob")
	if pending.State != "repair_pending" {
		t.Fatalf("repair intent lost: %+v", pending)
	}
	r = New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := r.Work(ctx); err != nil {
		t.Fatal(err)
	}
	repaired, _, _ := db.Replica(lid, "bob")
	a, err := api.GetAsset(ctx, c.Members[1], repaired.AssetID)
	if err != nil || a.IsTrashed || repaired.State != "ready" {
		t.Fatalf("retry failed: %+v %+v %v", repaired, a, err)
	}
}
