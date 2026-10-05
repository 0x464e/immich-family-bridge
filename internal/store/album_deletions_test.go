package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/domain"
)

func TestAlbumRecoveryAndMarkerBaselineSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	members := []domain.Member{{ID: "a", UserID: "u-a", LibraryID: "l-a"}, {ID: "b", UserID: "u-b", LibraryID: "l-b"}}
	if err := s.Init("family", members); err != nil {
		t.Fatal(err)
	}
	for _, a := range []domain.LogicalAlbum{{ID: "t", Name: "Together", SystemKey: "together"}, {ID: "x", Name: "Trip", Description: "Hello"}} {
		if err := s.AddAlbum("family", a, map[string]string{"a": a.ID + "-a", "b": a.ID + "-b"}); err != nil {
			t.Fatal(err)
		}
	}
	lid, err := s.EnsureOrigin("family", "a", domain.Asset{ID: "asset", OwnerID: "u-a", OriginalPath: "/fixture/a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMembership("x", lid, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAlbumMarker("t", "a", "marker"); err != nil {
		t.Fatal(err)
	}
	record := domain.AlbumDeletion{Album: domain.LogicalAlbum{ID: "x", Name: "Trip", Description: "Hello"}, DeletedAt: "2026-10-05T00:00:00Z", RequestedBy: "api", State: "deleting", Replicas: []domain.DeletedAlbumReplica{{MemberID: "a", AlbumID: "x-a", AssetIDs: []string{"asset"}}}}
	if err := s.BeginAlbumDeletion(record); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, found, err := s.AlbumDeletion("x")
	if err != nil || !found || got.Album.Name != "Trip" || got.Replicas[0].AssetIDs[0] != "asset" || got.State != "deleting" {
		t.Fatalf("archive lost: %+v %v", got, err)
	}
	if observed, err := s.ObservedAlbumMarker("t", "a", "marker"); err != nil || !observed {
		t.Fatal("marker baseline lost")
	}
	if albums, err := s.Albums(); err != nil || len(albums) != 1 || albums[0].SystemKey != "together" {
		t.Fatal("archive still active")
	}
	if _, found, err := s.ResolveMirrorAlbum(context.Background(), "x-a", "u-b"); err != nil || found {
		t.Fatal("archived link resolves")
	}
	if together, err := s.Memberships("t"); err != nil || !together[lid] {
		t.Fatal("secondary-only sharing not preserved in Together")
	}
	if own, err := s.Memberships("x"); err != nil || len(own) != 0 {
		t.Fatal("archived memberships remain active")
	}
	if err := s.UpdateAlbum("x", "Changed", "", ""); err == nil {
		t.Fatal("archived metadata editable")
	}
	if err := s.BeginAlbumDeletion(domain.AlbumDeletion{Album: domain.LogicalAlbum{ID: "t"}}); err == nil {
		t.Fatal("store accepted system album deletion")
	}
}
