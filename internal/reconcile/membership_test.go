package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/immich"
)

type albumFailure struct {
	immich.Client
	album string
	write bool
}

func (f albumFailure) GetAlbum(ctx context.Context, m domain.Member, id string) (domain.Album, error) {
	if !f.write && id == f.album {
		return domain.Album{}, errors.New("test outage")
	}
	return f.Client.GetAlbum(ctx, m, id)
}
func (f albumFailure) RemoveAssets(ctx context.Context, m domain.Member, id string, ids []string) error {
	if f.write && id == f.album {
		return errors.New("test write outage")
	}
	return f.Client.RemoveAssets(ctx, m, id, ids)
}

func TestTogetherAuthorityAndRestart(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	together, err := r.EnsureTogether(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trip, err := r.Register(ctx, "alice", "trip")
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	run()
	lid, _, _ := db.FindReplicaAsset("alice", "a1")
	check := func(album string, want bool) {
		t.Helper()
		membership, err := db.Memberships(album)
		if err != nil || membership[lid] != want {
			t.Fatalf("membership %s: %v %v", album, membership, err)
		}
		reps, _ := db.AlbumReplicas(album)
		for _, m := range c.Members {
			assets, err := api.ListAlbumAssets(ctx, m, reps[m.ID])
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, asset := range assets {
				logical, _, _ := db.FindReplicaAsset(m.ID, asset.ID)
				found = found || logical == lid
			}
			if found != want {
				t.Fatalf("%s %s present=%v want=%v", album, m.ID, found, want)
			}
		}
	}
	check(together, true)
	check(trip, true)
	// Removing from a secondary album preserves Together and all replicas.
	if err := api.RemoveAssets(ctx, c.Members[0], "trip", []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	run()
	check(trip, false)
	check(together, true)
	if err := api.AddAssets(ctx, c.Members[0], "trip", []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	run()
	togetherReps, _ := db.AlbumReplicas(together)
	bob, _, _ := db.Replica(lid, "bob")
	if err := api.RemoveAssets(ctx, c.Members[1], togetherReps["bob"], []string{bob.AssetID}); err != nil {
		t.Fatal(err)
	}
	preview, err := r.DryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, a := range preview {
		if a.Kind == "remove_sharing_reference" && a.LogicalAssetID == lid {
			removed[a.AlbumID] = true
		}
	}
	if !removed[together] || !removed[trip] {
		t.Fatalf("preview missed global unshare: %+v", preview)
	}
	// An unavailable secondary album prevents any membership decision.
	r.API = albumFailure{Client: api, album: "trip"}
	if err := r.Run(ctx); err == nil {
		t.Fatal("expected read failure")
	}
	desired, _ := db.Memberships(together)
	if !desired[lid] {
		t.Fatal("incomplete read changed Together")
	}
	// A remote write failure after the atomic decision must not resurrect the
	// asset from the secondary album's unchanged membership after restart.
	r.API = albumFailure{Client: api, album: "trip", write: true}
	if err := r.Run(ctx); err == nil {
		t.Fatal("expected write failure")
	}
	r = New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run()
	check(together, false)
	check(trip, false)
	run()
	check(together, false)
	check(trip, false)
	origin, err := api.GetAsset(ctx, c.Members[0], "a1")
	if err != nil || origin.ID != "a1" {
		t.Fatal("origin lost", err)
	}
	// A later deliberate secondary addition shares the asset again.
	if err := api.AddAssets(ctx, c.Members[0], "trip", []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	r.scanInterval = 0
	run()
	check(together, true)
	check(trip, true)
}

func TestTogetherBackfillsExistingSecondaryMembership(t *testing.T) {
	_, db, _, r := setup(t)
	ctx := context.Background()
	trip, err := r.Register(ctx, "alice", "trip")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	lid, _, _ := db.FindReplicaAsset("alice", "a1")
	together, err := r.EnsureTogether(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, album := range []string{trip, together} {
		desired, err := db.Memberships(album)
		if err != nil || !desired[lid] {
			t.Fatalf("missing upgrade membership %s %v", album, err)
		}
	}
}

func TestTogetherConflictPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		directAdd bool
		want      bool
	}{
		{"secondary addition does not override unshare", false, false},
		{"direct Together addition wins concurrent removal", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			together := &albumPlan{album: domain.LogicalAlbum{ID: "t", SystemKey: "together"}, desired: map[string]bool{}, adds: map[string]bool{}, removes: map[string]bool{"a": true}}
			if tc.directAdd {
				together.adds["a"] = true
				together.desired["a"] = true
			}
			secondary := &albumPlan{album: domain.LogicalAlbum{ID: "s"}, desired: map[string]bool{"a": true}, adds: map[string]bool{"a": true}}
			plans := []*albumPlan{secondary, together}
			resolveTogether(plans)
			if together.desired["a"] != tc.want || secondary.desired["a"] != tc.want || plans[0] != together {
				t.Fatal("unexpected conflict resolution")
			}
		})
	}
}

func TestTogetherDryRunPromotesNewSecondaryAssetWithoutWrites(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	c.DryRun = true
	r = New(c, db, api, slog.New(slog.NewTextHandler(io.Discard, nil)))
	together, err := r.EnsureTogether(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	actions, err := r.DryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	promoted := false
	for _, a := range actions {
		if a.AlbumID == together && a.Kind == "add_album_asset" && a.MemberID == "alice" && a.ImmichAssetID == "a1" {
			promoted = true
		}
	}
	if !promoted {
		t.Fatalf("Together addition absent from dry-run: %+v", actions)
	}
	replicas, err := db.Replicas()
	if err != nil || len(replicas) != 0 {
		t.Fatalf("dry run wrote replicas: %+v %v", replicas, err)
	}
	desired, _ := db.Memberships(together)
	if len(desired) != 0 {
		t.Fatal("dry run wrote Together membership")
	}
}

type delayedFirstLookup struct {
	immich.Client
	calls map[string]int
}

func (d *delayedFirstLookup) FindByPath(ctx context.Context, m domain.Member, path string) ([]domain.Asset, error) {
	d.calls[m.ID]++
	if d.calls[m.ID] == 1 {
		return nil, nil
	}
	return d.Client.FindByPath(ctx, m, path)
}
func TestSecondaryAddWaitsForTogether(t *testing.T) {
	_, db, api, r := setup(t)
	ctx := context.Background()
	together, err := r.EnsureTogether(ctx)
	if err != nil {
		t.Fatal(err)
	}
	trip, err := r.Register(ctx, "alice", "trip")
	if err != nil {
		t.Fatal(err)
	}
	r.API = &delayedFirstLookup{Client: api, calls: map[string]int{}}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	trips, _ := db.AlbumReplicas(trip)
	for _, m := range r.C.Members[1:] {
		assets, err := api.ListAlbumAssets(ctx, m, trips[m.ID])
		if err != nil || len(assets) != 0 {
			t.Fatalf("secondary added before Together: %s %+v %v", m.ID, assets, err)
		}
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, album := range []string{together, trip} {
		reps, _ := db.AlbumReplicas(album)
		for _, m := range r.C.Members {
			assets, err := api.ListAlbumAssets(ctx, m, reps[m.ID])
			if err != nil || len(assets) != 1 {
				t.Fatalf("did not converge %s %s: %+v %v", album, m.ID, assets, err)
			}
		}
	}
}
