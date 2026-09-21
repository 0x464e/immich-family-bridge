package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func addRawJPEGPair(t *testing.T, cpath string, api interface {
	MoveAssetPath(string, string) error
	AddAsset(string, string, string) error
}) (string, string) {
	t.Helper()
	jpeg := filepath.Join(filepath.Dir(cpath), "expert-raw.jpeg")
	dng := filepath.Join(filepath.Dir(cpath), "expert-raw.dng")
	if err := os.Rename(cpath, jpeg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dng, []byte("raw"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := api.MoveAssetPath("a1", jpeg); err != nil {
		t.Fatal(err)
	}
	if err := api.AddAsset("a2", "alice", dng); err != nil {
		t.Fatal(err)
	}
	return jpeg, dng
}

func TestRetroactiveStackMirrorsReadyRawJPEGPair(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	_, _ = addRawJPEGPair(t, filepath.Join(c.SourceRoot, "a.jpg"), api)
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	if err := api.AddAssets(ctx, c.Members[0], "trip", []string{"a2"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := api.SetStack("alice", "source-stack", []string{"a1", "a2"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, member := range c.Members[1:] {
		replica, found, err := db.StackReplica("alice", "source-stack", member.ID)
		if err != nil || !found || replica.State != "ready" || replica.StackID == "" {
			t.Fatalf("stack replica for %s: %+v found=%v err=%v", member.ID, replica, found, err)
		}
		stack, err := api.GetStack(ctx, member, replica.StackID)
		if err != nil || len(stack.Assets) != 2 {
			t.Fatalf("recipient stack for %s: %+v %v", member.ID, stack, err)
		}
		jpeg, _, _ := db.Replica(mustLogical(t, db, "alice", "a1"), member.ID)
		if stack.PrimaryAssetID != jpeg.AssetID {
			t.Fatalf("recipient stack primary for %s = %s, want JPEG %s", member.ID, stack.PrimaryAssetID, jpeg.AssetID)
		}
	}
	// A source primary change recreates the recipient relationship but leaves
	// the already-imported asset replicas untouched.
	if err := api.SetStack("alice", "source-stack", []string{"a2", "a1"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, member := range c.Members[1:] {
		replica, _, _ := db.StackReplica("alice", "source-stack", member.ID)
		stack, err := api.GetStack(ctx, member, replica.StackID)
		dng, _, _ := db.Replica(mustLogical(t, db, "alice", "a2"), member.ID)
		if err != nil || stack.PrimaryAssetID != dng.AssetID {
			t.Fatalf("primary change not mirrored for %s: %+v %v", member.ID, stack, err)
		}
	}
	if err := api.DeleteStack(ctx, c.Members[0], "source-stack"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if stacks, err := db.SourceStacks(); err != nil || len(stacks) != 0 {
		t.Fatalf("source stack was not removed: %+v %v", stacks, err)
	}
}

func TestStackWaitsForEveryReplicaAndNeverCreatesPartialStack(t *testing.T) {
	c, db, api, r := setup(t)
	ctx := context.Background()
	_, _ = addRawJPEGPair(t, filepath.Join(c.SourceRoot, "a.jpg"), api)
	if _, err := r.Register(ctx, "alice", "trip"); err != nil {
		t.Fatal(err)
	}
	if err := api.AddAssets(ctx, c.Members[0], "trip", []string{"a2"}); err != nil {
		t.Fatal(err)
	}
	if err := api.SetStack("alice", "source-stack", []string{"a1", "a2"}); err != nil {
		t.Fatal(err)
	}
	api.BlockScans(true)
	r.scanInterval = 0
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, member := range c.Members[1:] {
		replica, found, err := db.StackReplica("alice", "source-stack", member.ID)
		if err != nil || !found || replica.State != "pending" || replica.StackID != "" {
			t.Fatalf("partial stack was created for %s: %+v found=%v err=%v", member.ID, replica, found, err)
		}
	}
	api.BlockScans(false)
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, member := range c.Members[1:] {
		replica, found, err := db.StackReplica("alice", "source-stack", member.ID)
		if err != nil || !found || replica.State != "ready" {
			t.Fatalf("stack did not recover for %s: %+v found=%v err=%v", member.ID, replica, found, err)
		}
	}
	// Add an unshared third source member. The existing recipient stack must be
	// removed rather than become a partial two-asset stack.
	extra := filepath.Join(c.SourceRoot, "unshared.jpg")
	if err := os.WriteFile(extra, []byte("unshared"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := api.AddAsset("a3", "alice", extra); err != nil {
		t.Fatal(err)
	}
	if err := api.SetStack("alice", "source-stack", []string{"a1", "a2", "a3"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, member := range c.Members[1:] {
		replica, _, _ := db.StackReplica("alice", "source-stack", member.ID)
		if replica.State != "pending" || replica.StackID != "" {
			t.Fatalf("incomplete source stack remained mirrored for %s: %+v", member.ID, replica)
		}
	}
}

func mustLogical(t *testing.T, db interface {
	FindReplicaAsset(string, string) (string, bool, error)
}, member, asset string) string {
	t.Helper()
	id, found, err := db.FindReplicaAsset(member, asset)
	if err != nil || !found {
		t.Fatalf("logical asset %s/%s: %q %v", member, asset, id, err)
	}
	return id
}
