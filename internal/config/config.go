// Package config loads gh-mirror settings and resolves user directories.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gh-mirror/internal/commentpolicy"
	"gh-mirror/internal/embedding"
	"gh-mirror/internal/representation"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// DefaultWorkers bounds concurrent GitHub requests in a normal sync.
const DefaultWorkers = 4

// MaxWorkers caps user-configured fetch concurrency.
const MaxWorkers = 16

// Config defines collection scope, paths, and environment variable names.
type Config struct {
	PRComments         commentpolicy.Policy                  `json:"pr_comments,omitempty"`
	PRCommentOverrides map[string]commentpolicy.Override     `json:"pr_comment_overrides,omitempty"`
	LabelRules         map[string][]representation.LabelRule `json:"label_rules,omitempty"`
	Repositories       []string                              `json:"repositories"`
	RepositoryOptions  map[string]Scope                      `json:"repository_options,omitempty"`
	Database           string                                `json:"database"`
	SnapshotDir        string                                `json:"snapshot_dir"`
	APIURL             string                                `json:"api_url"`
	GraphQLURL         string                                `json:"graphql_url"`
	TokenEnv           string                                `json:"token_env"`
	APITokenEnv        string                                `json:"api_token_env"`
	Listen             string                                `json:"listen"`
	Fields             bool                                  `json:"fields"`
	Projects           bool                                  `json:"projects"`
	MaxRequests        int                                   `json:"max_requests"`
	Workers            int                                   `json:"workers"`
	Embedding          embedding.Options                     `json:"embedding,omitempty"`
	Overlap            string                                `json:"overlap"`
	EnrichmentInterval string                                `json:"enrichment_interval"`
	ReconcileInterval  string                                `json:"reconcile_interval"`
}

// DefaultPath prefers TOML and falls back to an existing legacy JSON configuration.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config directory: %w", err)
	}
	dir = filepath.Join(dir, "gh-mirror")
	path := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect TOML configuration: %w", err)
	}
	legacy := filepath.Join(dir, "config.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect legacy configuration: %w", err)
	}
	return path, nil
}

func defaults() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home directory: %w", err)
	}
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		dataDir = filepath.Join(home, ".local", "share")
	}
	dataDir = filepath.Join(dataDir, "gh-mirror")
	c := Config{LabelRules: map[string][]representation.LabelRule{}, Database: filepath.Join(dataDir, "state.sqlite"), SnapshotDir: filepath.Join(dataDir, "snapshots"), APIURL: "https://api.github.com", GraphQLURL: "https://api.github.com/graphql", TokenEnv: "GITHUB_TOKEN", APITokenEnv: "GH_MIRROR_API_TOKEN", Listen: "127.0.0.1:8787", Fields: true, Projects: true, MaxRequests: 3000, Workers: DefaultWorkers, Overlap: "5m", EnrichmentInterval: "1h", ReconcileInterval: "24h"}
	return c, nil
}

// Load reads TOML or legacy JSON; an absent default file uses built-in settings.
func Load(path string, explicit bool) (Config, error) {
	c, err := defaults()
	if err != nil {
		return c, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return c, nil
		}
		return c, fmt.Errorf("open config: %w", err)
	}
	var reader io.Reader = f
	if strings.EqualFold(filepath.Ext(path), ".toml") {
		raw, err := decodeTOML(f, c)
		if err != nil {
			if closeErr := f.Close(); closeErr != nil {
				return c, fmt.Errorf("decode TOML: %w; close config: %w", err, closeErr)
			}
			return c, fmt.Errorf("decode TOML: %w", err)
		}
		reader = strings.NewReader(string(raw))
	}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var raw json.RawMessage
	decodeErr := decoder.Decode(&raw)
	if decodeErr == nil {
		if len(raw) == 0 || raw[0] != '{' {
			decodeErr = fmt.Errorf("expected JSON configuration object")
		} else {
			objectDecoder := json.NewDecoder(strings.NewReader(string(raw)))
			objectDecoder.DisallowUnknownFields()
			decodeErr = objectDecoder.Decode(&c)
		}
	}
	if decodeErr == nil {
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			decodeErr = fmt.Errorf("expected one JSON object")
		}
	}
	closeErr := f.Close()
	if decodeErr != nil {
		return c, fmt.Errorf("decode config: %w", decodeErr)
	}
	if closeErr != nil {
		return c, fmt.Errorf("close config: %w", closeErr)
	}
	return c, nil
}

// Validate checks settings before any database or network operation.
func (c Config) Validate() error {
	if err := c.PRComments.Validate(); err != nil {
		return fmt.Errorf("PR comment policy: %w", err)
	}
	for repo, override := range c.PRCommentOverrides {
		if !slices.Contains(c.Repositories, repo) {
			return fmt.Errorf("PR comment override %q must match a configured repository", repo)
		}
		if err := c.PRComments.Resolve(override).Validate(); err != nil {
			return fmt.Errorf("PR comment policy for %s: %w", repo, err)
		}
	}
	if err := representation.ValidateRules(c.LabelRules); err != nil {
		return fmt.Errorf("label rules: %w", err)
	}
	for repo := range c.LabelRules {
		if !ValidRepository(repo) {
			return fmt.Errorf("invalid label_rules repository %q", repo)
		}
	}
	if err := c.Embedding.Validate(); err != nil {
		return fmt.Errorf("embedding settings: %w", err)
	}
	if c.Workers < 1 || c.Workers > MaxWorkers {
		return fmt.Errorf("workers must be between 1 and %d", MaxWorkers)
	}
	seen := map[string]bool{}
	for _, repo := range c.Repositories {
		if !ValidRepository(repo) || seen[strings.ToLower(repo)] {
			return fmt.Errorf("invalid or repeated repository %q", repo)
		}
		seen[strings.ToLower(repo)] = true
	}
	for repo := range c.RepositoryOptions {
		found := false
		for _, configured := range c.Repositories {
			if repo == configured {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("repository_options key %q must match a configured repository", repo)
		}
		scope := c.Scope(repo)
		if !scope.Issues && !scope.PullRequests {
			return fmt.Errorf("repository %s must enable issues or pull_requests", repo)
		}
	}
	if c.Database == "" || c.SnapshotDir == "" || c.TokenEnv == "" || c.APITokenEnv == "" || c.MaxRequests < 1 {
		return fmt.Errorf("paths, token environment names and positive max_requests are required")
	}
	for _, raw := range []string{c.APIURL, c.GraphQLURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("invalid upstream API URL")
		}
		if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
			return fmt.Errorf("upstream HTTP is allowed only on loopback")
		}
	}
	for name, value := range map[string]string{"overlap": c.Overlap, "enrichment_interval": c.EnrichmentInterval, "reconcile_interval": c.ReconcileInterval} {
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s must be a positive duration", name)
		}
	}
	return nil
}

// CommentPolicies resolves PR retrieval policies independently of collection scope.
func (c Config) CommentPolicies() map[string]commentpolicy.Policy {
	out := make(map[string]commentpolicy.Policy, len(c.Repositories))
	for _, repo := range c.Repositories {
		out[repo] = c.PRComments.Resolve(c.PRCommentOverrides[repo])
	}
	return out
}

// ValidRepository reports whether a name has a usable owner/repository shape.
func ValidRepository(repo string) bool {
	if !repositoryPattern.MatchString(repo) {
		return false
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}
