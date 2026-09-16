package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"gopkg.in/yaml.v3"
)

type FakeAsset struct {
	ID     string `yaml:"id"`
	Member string `yaml:"member"`
	Path   string `yaml:"path"`
	Type   string `yaml:"type"`
}
type FakeAlbum struct {
	ID          string   `yaml:"id"`
	Member      string   `yaml:"member"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	AssetIDs    []string `yaml:"asset_ids"`
}
type Fake struct {
	Assets []FakeAsset `yaml:"assets"`
	Albums []FakeAlbum `yaml:"albums"`
}
type Config struct {
	Mode             string          `yaml:"mode"`
	FamilyID         string          `yaml:"family_id"`
	ImmichURL        string          `yaml:"immich_url"`
	AdminKeyEnv      string          `yaml:"admin_key_env"`
	AdminKey         string          `yaml:"-"`
	SourceRoot       string          `yaml:"source_root"`
	BridgeRoot       string          `yaml:"bridge_root"`
	ImmichBridgeRoot string          `yaml:"immich_bridge_root"`
	Database         string          `yaml:"database"`
	FakeState        string          `yaml:"fake_state"`
	Listen           string          `yaml:"listen"`
	APITokenEnv      string          `yaml:"api_token_env"`
	APIToken         string          `yaml:"-"`
	PollInterval     string          `yaml:"poll_interval"`
	Members          []domain.Member `yaml:"members"`
	Fake             Fake            `yaml:"fake"`
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, err
	}
	if c.Mode == "" {
		c.Mode = "fake"
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.PollInterval == "" {
		c.PollInterval = "30s"
	}
	if c.APITokenEnv != "" {
		c.APIToken = os.Getenv(c.APITokenEnv)
	}
	if c.AdminKeyEnv != "" {
		c.AdminKey = os.Getenv(c.AdminKeyEnv)
	}
	for i := range c.Members {
		if c.Members[i].KeyEnv != "" {
			c.Members[i].Key = os.Getenv(c.Members[i].KeyEnv)
		}
	}
	return c, c.Validate()
}

func (c Config) Interval() (time.Duration, error) { return time.ParseDuration(c.PollInterval) }

func (c Config) Validate() error {
	if c.Mode != "fake" {
		return errors.New("only fake mode is enabled; real Immich integration awaits disposable-instance validation")
	}
	if !safeID.MatchString(c.FamilyID) {
		return errors.New("invalid family_id")
	}
	if c.Database == "" || !filepath.IsAbs(c.Database) {
		return errors.New("database must be an absolute path")
	}
	if c.SourceRoot == "" || !filepath.IsAbs(c.SourceRoot) || c.BridgeRoot == "" || !filepath.IsAbs(c.BridgeRoot) {
		return errors.New("source_root and bridge_root must be absolute")
	}
	if c.ImmichBridgeRoot == "" || !filepath.IsAbs(c.ImmichBridgeRoot) {
		return errors.New("immich_bridge_root must be absolute")
	}
	if c.FakeState == "" || !filepath.IsAbs(c.FakeState) {
		return errors.New("fake_state must be an absolute path")
	}
	if c.APITokenEnv == "" || c.APIToken == "" {
		return errors.New("api_token_env must name a nonempty environment variable")
	}
	if c.SourceRoot == c.BridgeRoot || inside(c.SourceRoot, c.BridgeRoot) || inside(c.BridgeRoot, c.SourceRoot) {
		return errors.New("source_root and bridge_root must not overlap")
	}
	if inside(c.SourceRoot, c.Database) || inside(c.BridgeRoot, c.Database) || inside(c.SourceRoot, c.FakeState) || inside(c.BridgeRoot, c.FakeState) {
		return errors.New("state files must be outside source and bridge roots")
	}
	if filepath.Base(c.SourceRoot) != "source" || filepath.Base(c.BridgeRoot) != "bridge" || filepath.Dir(c.SourceRoot) != filepath.Dir(c.BridgeRoot) {
		return errors.New("fake mode requires sibling source and bridge fixture directories")
	}
	marker, err := os.ReadFile(filepath.Join(filepath.Dir(c.SourceRoot), ".familybridge-fixture"))
	if err != nil || strings.TrimSpace(string(marker)) != "familybridge-test-fixture-v1" {
		return errors.New("fake fixture marker missing or invalid")
	}
	if info, e := os.Lstat(c.SourceRoot); e != nil || !info.IsDir() {
		return errors.New("fixture source_root must exist as a directory")
	}
	if info, e := os.Lstat(c.BridgeRoot); e == nil && !info.IsDir() {
		return errors.New("fixture bridge_root must be a directory")
	}
	d, err := c.Interval()
	if err != nil || d < time.Second {
		return errors.New("poll_interval must be at least 1s")
	}
	if len(c.Members) < 2 {
		return errors.New("at least two members are required")
	}
	ids := map[string]bool{}
	users := map[string]bool{}
	libraries := map[string]bool{}
	for _, m := range c.Members {
		if !safeID.MatchString(m.ID) || m.UserID == "" || m.LibraryID == "" {
			return fmt.Errorf("invalid member %q", m.ID)
		}
		if ids[m.ID] || users[m.UserID] || libraries[m.LibraryID] {
			return errors.New("member ids, user ids, and library ids must each be unique")
		}
		ids[m.ID] = true
		users[m.UserID] = true
		libraries[m.LibraryID] = true
	}
	for _, a := range c.Fake.Assets {
		if !ids[a.Member] || a.ID == "" || !inside(c.SourceRoot, a.Path) {
			return fmt.Errorf("invalid fake asset %q", a.ID)
		}
		info, e := os.Lstat(a.Path)
		if e != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("fake asset %q must be a regular file", a.ID)
		}
	}
	for _, a := range c.Fake.Albums {
		if !ids[a.Member] || a.ID == "" || a.Name == "" {
			return fmt.Errorf("invalid fake album %q", a.ID)
		}
	}
	return nil
}

func inside(root, path string) bool {
	r, e := filepath.Rel(root, path)
	return e == nil && r != "." && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
