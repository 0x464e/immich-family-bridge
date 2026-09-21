package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/0x464e/immich-family-bridge/internal/domain"
)

func TestLoadDefaultsToDryRun(t *testing.T) {
	previous, wasSet := os.LookupEnv("FAMILYBRIDGE_DRY_RUN")
	if err := os.Unsetenv("FAMILYBRIDGE_DRY_RUN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv("FAMILYBRIDGE_DRY_RUN", previous)
		} else {
			_ = os.Unsetenv("FAMILYBRIDGE_DRY_RUN")
		}
	})
	media := t.TempDir()
	upload := filepath.Join(media, "upload")
	if err := os.Mkdir(upload, 0750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TEST_ADMIN_KEY", "TEST_API_TOKEN", "TEST_FORWARDAUTH_TOKEN", "TEST_ALICE_KEY", "TEST_BOB_KEY"} {
		t.Setenv(name, "test-secret")
	}
	base := fmt.Sprintf(`family_id: family
immich_url: http://immich-server:2283/api
admin_key_env: TEST_ADMIN_KEY
source_root: %s
source_mappings:
  - immich_root: /data
    local_root: %s
bridge_root: %s
immich_bridge_root: /bridge
database: %s
api_token_env: TEST_API_TOKEN
forward_auth_token_env: TEST_FORWARDAUTH_TOKEN
members:
  - id: alice
    user_id: u-alice
    library_id: l-alice
    key_env: TEST_ALICE_KEY
  - id: bob
    user_id: u-bob
    library_id: l-bob
    key_env: TEST_BOB_KEY
`, media, upload, filepath.Join(media, "bridge"), filepath.Join(t.TempDir(), "bridge.sqlite"))
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct {
		name, env string
		want      bool
		wantError bool
	}{{name: "omitted", want: true}, {name: "explicit true", env: "true", want: true}, {name: "explicit false", env: "false"}, {name: "invalid", env: "maybe", wantError: true}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("FAMILYBRIDGE_DRY_RUN", tc.env)
			}
			if err := os.WriteFile(path, []byte(base), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if tc.wantError {
				if err == nil {
					t.Fatal("accepted invalid FAMILYBRIDGE_DRY_RUN")
				}
				return
			}
			if err != nil || c.DryRun != tc.want {
				t.Fatalf("DryRun=%v, err=%v, want %v", c.DryRun, err, tc.want)
			}
			if c.Listen != "127.0.0.1:6773" {
				t.Fatalf("Listen=%q, want default 127.0.0.1:6773", c.Listen)
			}
			if c.TogetherAlbumName != "Together" {
				t.Fatalf("TogetherAlbumName=%q, want default Together", c.TogetherAlbumName)
			}
			if got, err := c.DiscoveryEvery(); err != nil || got.String() != "5m0s" {
				t.Fatalf("DiscoveryEvery=%v, %v, want 5m", got, err)
			}
		})
	}
	if err := os.WriteFile(path, []byte("dry_run: false\n"+base), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted obsolete dry_run YAML field")
	}
	if err := os.WriteFile(path, []byte("together_album_name: Family Room\n"+base), 0600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(path); err != nil || c.TogetherAlbumName != "Family Room" {
		t.Fatalf("custom Together album name not loaded: %q, %v", c.TogetherAlbumName, err)
	}
}

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
		APITokenEnv: "API_TOKEN", APIToken: "secret", ForwardAuthTokenEnv: "FORWARDAUTH_TOKEN", ForwardAuthToken: "secret", PollInterval: "30s", TogetherAlbumName: "Together",
		Members: []domain.Member{{ID: "alice", UserID: "u1", LibraryID: "l1", KeyEnv: "KEY1", Key: "secret"}, {ID: "bob", UserID: "u2", LibraryID: "l2", KeyEnv: "KEY2", Key: "secret"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.DiscoveryInterval = "bad"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted invalid discovery interval")
	}
	c.DiscoveryInterval = "5m"
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
	c.SourceMappings[1].LocalRoot = external
	c.ForwardAuthToken = "not url-safe"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted non URL-safe forward-auth token")
	}
}
