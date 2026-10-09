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
	"strings"
	"time"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// DefaultWorkers bounds concurrent GitHub requests in a normal sync.
const DefaultWorkers = 4

// MaxWorkers caps user-configured fetch concurrency.
const MaxWorkers = 16

// Config defines collection scope, paths, and environment variable names.
type Config struct {
	Repositories       []string `json:"repositories"`
	Database           string   `json:"database"`
	SnapshotDir        string   `json:"snapshot_dir"`
	APIURL             string   `json:"api_url"`
	GraphQLURL         string   `json:"graphql_url"`
	TokenEnv           string   `json:"token_env"`
	APITokenEnv        string   `json:"api_token_env"`
	Listen             string   `json:"listen"`
	Fields             bool     `json:"fields"`
	Projects           bool     `json:"projects"`
	MaxRequests        int      `json:"max_requests"`
	Workers            int      `json:"workers"`
	Overlap            string   `json:"overlap"`
	EnrichmentInterval string   `json:"enrichment_interval"`
	ReconcileInterval  string   `json:"reconcile_interval"`
}

// DefaultPath returns the XDG configuration filename.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config directory: %w", err)
	}
	return filepath.Join(dir, "gh-mirror", "config.json"), nil
}

// Load reads JSON configuration; an absent default file uses built-in settings.
func Load(path string, explicit bool) (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home directory: %w", err)
	}
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		dataDir = filepath.Join(home, ".local", "share")
	}
	dataDir = filepath.Join(dataDir, "gh-mirror")
	c := Config{Database: filepath.Join(dataDir, "state.sqlite"), SnapshotDir: filepath.Join(dataDir, "snapshots"), APIURL: "https://api.github.com", GraphQLURL: "https://api.github.com/graphql", TokenEnv: "GITHUB_TOKEN", APITokenEnv: "GH_MIRROR_API_TOKEN", Listen: "127.0.0.1:8787", Fields: true, Projects: true, MaxRequests: 3000, Workers: DefaultWorkers, Overlap: "5m", EnrichmentInterval: "1h", ReconcileInterval: "24h"}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return c, nil
		}
		return c, fmt.Errorf("open config: %w", err)
	}
	decoder := json.NewDecoder(f)
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
