package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/domain"
	"gopkg.in/yaml.v3"
)

type SourceMapping struct {
	ImmichRoot string `yaml:"immich_root"`
	LocalRoot  string `yaml:"local_root"`
}

type Config struct {
	FamilyID         string          `yaml:"family_id"`
	DryRun           bool            `yaml:"dry_run"`
	ImmichURL        string          `yaml:"immich_url"`
	AdminKeyEnv      string          `yaml:"admin_key_env"`
	AdminKey         string          `yaml:"-"`
	SourceRoot       string          `yaml:"source_root"`
	SourceMappings   []SourceMapping `yaml:"source_mappings"`
	BridgeRoot       string          `yaml:"bridge_root"`
	ImmichBridgeRoot string          `yaml:"immich_bridge_root"`
	Database         string          `yaml:"database"`
	Listen           string          `yaml:"listen"`
	APITokenEnv      string          `yaml:"api_token_env"`
	APIToken         string          `yaml:"-"`
	PollInterval     string          `yaml:"poll_interval"`
	Members          []domain.Member `yaml:"members"`
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c := Config{DryRun: true}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, err
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
	if !safeID.MatchString(c.FamilyID) {
		return errors.New("invalid family_id")
	}
	u, err := url.Parse(c.ImmichURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "/api" {
		return errors.New("immich_url must be an http(s) URL ending in /api")
	}
	if c.Database == "" || !filepath.IsAbs(c.Database) {
		return errors.New("database must be an absolute path")
	}
	for name, path := range map[string]string{"source_root": c.SourceRoot, "bridge_root": c.BridgeRoot, "immich_bridge_root": c.ImmichBridgeRoot} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return fmt.Errorf("%s must be a clean absolute non-root path", name)
		}
	}
	if c.APITokenEnv == "" || c.APIToken == "" || c.AdminKeyEnv == "" || c.AdminKey == "" {
		return errors.New("API token and admin key must resolve from environment variables")
	}
	if c.SourceRoot == c.BridgeRoot || inside(c.BridgeRoot, c.SourceRoot) {
		return errors.New("bridge_root must not contain source_root")
	}
	if inside(c.SourceRoot, c.Database) || inside(c.BridgeRoot, c.Database) {
		return errors.New("database must be outside source and bridge roots")
	}
	if info, e := os.Lstat(c.SourceRoot); e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("source_root must be an existing ordinary directory")
	}
	if info, e := os.Lstat(c.BridgeRoot); e == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("bridge_root must be an ordinary directory")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return fmt.Errorf("bridge_root: %w", e)
	}
	if len(c.SourceMappings) == 0 {
		return errors.New("at least one source_mappings entry is required")
	}
	for i, mapping := range c.SourceMappings {
		if !cleanNonRoot(mapping.ImmichRoot) || !cleanNonRoot(mapping.LocalRoot) || !(mapping.LocalRoot == c.SourceRoot || inside(c.SourceRoot, mapping.LocalRoot)) || mapping.LocalRoot == c.BridgeRoot || inside(mapping.LocalRoot, c.BridgeRoot) || inside(c.BridgeRoot, mapping.LocalRoot) {
			return fmt.Errorf("invalid source_mappings entry %d", i)
		}
		if info, err := os.Lstat(mapping.LocalRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("source_mappings entry %d local_root must be an existing ordinary directory", i)
		}
		for j := 0; j < i; j++ {
			other := c.SourceMappings[j].ImmichRoot
			if mapping.ImmichRoot == other || inside(mapping.ImmichRoot, other) || inside(other, mapping.ImmichRoot) {
				return errors.New("source_mappings Immich roots must not overlap")
			}
		}
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
		if !safeID.MatchString(m.ID) || m.UserID == "" || m.LibraryID == "" || m.KeyEnv == "" || m.Key == "" {
			return fmt.Errorf("invalid member %q or unresolved key", m.ID)
		}
		if ids[m.ID] || users[m.UserID] || libraries[m.LibraryID] {
			return errors.New("member ids, user ids, and library ids must each be unique")
		}
		ids[m.ID] = true
		users[m.UserID] = true
		libraries[m.LibraryID] = true
	}
	return nil
}

// SourcePath maps Immich's originalPath into this process's source mount.
func (c Config) SourcePath(immichPath string) (string, error) {
	if !filepath.IsAbs(immichPath) || filepath.Clean(immichPath) != immichPath {
		return "", errors.New("Immich originalPath is outside configured source root")
	}
	for _, mapping := range c.SourceMappings {
		if !inside(mapping.ImmichRoot, immichPath) {
			continue
		}
		rel, err := filepath.Rel(mapping.ImmichRoot, immichPath)
		if err != nil {
			return "", err
		}
		path := filepath.Join(mapping.LocalRoot, rel)
		if !inside(c.SourceRoot, path) {
			return "", errors.New("mapped source path escaped source root")
		}
		return path, nil
	}
	return "", errors.New("Immich originalPath has no configured source mapping")
}

func cleanNonRoot(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}

func inside(root, path string) bool {
	r, e := filepath.Rel(root, path)
	return e == nil && r != "." && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
