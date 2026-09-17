package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/domain"
)

func TestSourceMappingsAndValidation(t *testing.T) {
	media := t.TempDir()
	upload := filepath.Join(media, "immich-upload")
	external := filepath.Join(media, "external-sources")
	bridge := filepath.Join(media, "bridge-recipients")
	for _, dir := range []string{upload, external, bridge} {
		if err := os.Mkdir(dir, 0750); err != nil {
			t.Fatal(err)
		}
	}
	c := Config{
		FamilyID: "family", ImmichURL: "http://immich-server:2283/api", AdminKeyEnv: "ADMIN_KEY", AdminKey: "secret",
		SourceRoot: media, SourceMappings: []SourceMapping{{ImmichRoot: "/data", LocalRoot: upload}, {ImmichRoot: "/external", LocalRoot: external}},
		BridgeRoot: bridge, ImmichBridgeRoot: "/bridge", Database: filepath.Join(t.TempDir(), "bridge.sqlite"),
		APITokenEnv: "API_TOKEN", APIToken: "secret", PollInterval: "30s",
		Members: []domain.Member{{ID: "alice", UserID: "u1", LibraryID: "l1", KeyEnv: "KEY1", Key: "secret"}, {ID: "bob", UserID: "u2", LibraryID: "l2", KeyEnv: "KEY2", Key: "secret"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{
		"/data/library/photo.jpg": filepath.Join(upload, "library", "photo.jpg"),
		"/external/picture.jpg":   filepath.Join(external, "picture.jpg"),
	} {
		got, err := c.SourcePath(input)
		if err != nil || got != want {
			t.Fatalf("%s mapped to %s, %v; want %s", input, got, err, want)
		}
	}
	for _, input := range []string{"/bridge/photo.jpg", "/data/../bridge/photo.jpg", "relative/photo.jpg"} {
		if _, err := c.SourcePath(input); err == nil {
			t.Fatalf("accepted unsafe source path %s", input)
		}
	}
	c.SourceMappings[1].LocalRoot = bridge
	if err := c.Validate(); err == nil {
		t.Fatal("accepted a bridge output directory as a source mapping")
	}
}
