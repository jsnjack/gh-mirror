// Package snapshot publishes immutable mirrors and verifies private CI copies.
package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gh-mirror/internal/diagnostics"
	"gh-mirror/internal/store"
)

const maxSnapshot = 2 << 30

var filenamePattern = regexp.MustCompile(`^mirror-[A-Za-z0-9_-]+\.sqlite$`)

// Manifest pins a complete database file, checksum, collection time, and scope.
type Manifest struct {
	Filename             string   `json:"filename"`
	SHA256               string   `json:"sha256"`
	SchemaVersion        int      `json:"schema_version"`
	CollectionVersion    int      `json:"collection_version"`
	Generation           string   `json:"generation"`
	CollectedAt          string   `json:"collected_at"`
	EnrichedAt           string   `json:"enriched_at"`
	Repositories         []string `json:"repositories"`
	EmbeddingFingerprint string   `json:"embedding_fingerprint,omitempty"`
	EmbeddingDimension   int      `json:"embedding_dimension,omitempty"`
	IndexGeneration      string   `json:"index_generation,omitempty"`
}

func remove(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Log(context.Background(), diagnostics.TraceLevel, "remove temporary snapshot", "error", err)
	}
}
func closeFile(f *os.File) {
	if err := f.Close(); err != nil {
		slog.Log(context.Background(), diagnostics.TraceLevel, "close snapshot file", "error", err)
	}
}
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open snapshot directory: %w", err)
	}
	defer closeFile(f)
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync snapshot directory: %w", err)
	}
	return nil
}
func inspect(ctx context.Context, path string) (store.Status, error) {
	db, err := store.Open(path, true)
	if err != nil {
		return store.Status{}, fmt.Errorf("open snapshot for validation: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Log(ctx, diagnostics.TraceLevel, "close validated snapshot", "error", err)
		}
	}()
	if err := db.Validate(ctx); err != nil {
		return store.Status{}, fmt.Errorf("validate snapshot: %w", err)
	}
	s, err := db.Status(ctx)
	if err != nil {
		return s, fmt.Errorf("read snapshot metadata: %w", err)
	}
	if s.Generation == "" || s.CollectedAt == "" || len(s.Repositories) == 0 || len(s.Coverage) != len(s.Repositories) {
		return s, fmt.Errorf("snapshot has no complete committed collection")
	}
	if s.CollectionVersion != store.CollectionVersion {
		return s, fmt.Errorf("unsupported snapshot collection version %d; run sync with a compatible collector", s.CollectionVersion)
	}
	for _, c := range s.Coverage {
		if c.CollectedAt != s.CollectedAt || c.Fields == "" || c.Projects == "" {
			return s, fmt.Errorf("snapshot has incomplete repository coverage")
		}
	}
	return s, nil
}
func checksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open checksum file: %w", err)
	}
	defer closeFile(f)
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash snapshot: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Publish exports a consistent standalone database and atomically updates latest.json.
func Publish(ctx context.Context, db *store.Store, dir string) (Manifest, error) {
	var m Manifest
	if err := os.MkdirAll(dir, 0700); err != nil {
		return m, fmt.Errorf("create snapshot directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".export-*.sqlite")
	if err != nil {
		return m, fmt.Errorf("create export filename: %w", err)
	}
	temp := f.Name()
	if err := f.Close(); err != nil {
		return m, fmt.Errorf("close export placeholder: %w", err)
	}
	defer remove(temp)
	if err := os.Remove(temp); err != nil {
		return m, fmt.Errorf("remove export placeholder: %w", err)
	}
	if err := db.Export(ctx, temp); err != nil {
		return m, fmt.Errorf("publish mirror export: %w", err)
	}
	if err := os.Chmod(temp, 0600); err != nil {
		return m, fmt.Errorf("protect snapshot: %w", err)
	}
	s, err := inspect(ctx, temp)
	if err != nil {
		return m, fmt.Errorf("inspect exported mirror: %w", err)
	}
	hash, err := checksum(temp)
	if err != nil {
		return m, fmt.Errorf("checksum exported mirror: %w", err)
	}
	m = Manifest{Filename: "mirror-" + s.Generation + "-" + hash[:12] + ".sqlite", SHA256: hash, SchemaVersion: s.SchemaVersion, CollectionVersion: s.CollectionVersion, Generation: s.Generation, CollectedAt: s.CollectedAt, EnrichedAt: s.EnrichedAt, Repositories: s.Repositories}
	if s.Semantic != nil {
		m.EmbeddingFingerprint = s.Semantic.Fingerprint
		m.EmbeddingDimension = s.Semantic.Dimension
		m.IndexGeneration = s.Semantic.Generation
	}
	if !ValidFilename(m.Filename) {
		return m, fmt.Errorf("invalid snapshot generation identity")
	}
	f, err = os.OpenFile(temp, os.O_RDWR, 0600)
	if err != nil {
		return m, fmt.Errorf("open export for fsync: %w", err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return m, fmt.Errorf("fsync export: %w", syncErr)
	}
	if closeErr != nil {
		return m, fmt.Errorf("close fsynced export: %w", closeErr)
	}
	// Hard-link installation refuses to replace an immutable generation file.
	if err := os.Link(temp, filepath.Join(dir, m.Filename)); err != nil {
		if !os.IsExist(err) {
			return m, fmt.Errorf("install immutable snapshot: %w", err)
		}
		existing, hashErr := checksum(filepath.Join(dir, m.Filename))
		if hashErr != nil {
			return m, fmt.Errorf("verify existing snapshot: %w", hashErr)
		}
		if existing != m.SHA256 {
			return m, fmt.Errorf("immutable snapshot filename collision")
		}
	}
	if err := syncDir(dir); err != nil {
		return m, fmt.Errorf("persist immutable snapshot: %w", err)
	}
	encoded, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, fmt.Errorf("encode snapshot manifest: %w", err)
	}
	if err := atomicBytes(dir, "latest.json", append(encoded, '\n')); err != nil {
		return m, fmt.Errorf("publish manifest: %w", err)
	}
	return m, nil
}
func atomicBytes(dir, name string, body []byte) error {
	f, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	defer remove(f.Name())
	if _, err := f.Write(body); err != nil {
		closeFile(f)
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := f.Sync(); err != nil {
		closeFile(f)
		return fmt.Errorf("sync manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close manifest: %w", err)
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("replace manifest: %w", err)
	}
	return syncDir(dir)
}

// ReadManifest decodes a bounded manifest file served by the snapshot endpoint.
func ReadManifest(path string) (Manifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	return decode(body)
}
func decode(body []byte) (Manifest, error) {
	var m Manifest
	if len(body) > 1<<20 {
		return m, fmt.Errorf("manifest exceeds 1 MiB")
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if m.CollectionVersion == 0 {
		m.CollectionVersion = store.LegacyCollectionVersion
	}
	if m.CollectionVersion != store.CollectionVersion {
		return m, fmt.Errorf("unsupported snapshot collection version %d", m.CollectionVersion)
	}
	if !filenamePattern.MatchString(m.Filename) || m.Generation == "" || !store.CompatibleSchema(m.SchemaVersion) || len(m.Repositories) == 0 || len(m.SHA256) != 64 {
		return m, fmt.Errorf("invalid filename, checksum, schema or scope in manifest")
	}
	if _, err := hex.DecodeString(m.SHA256); err != nil {
		return m, fmt.Errorf("invalid snapshot checksum: %w", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, m.CollectedAt); err != nil {
		return m, fmt.Errorf("invalid snapshot timestamp: %w", err)
	}
	return m, nil
}

// Options constrains snapshot acquisition by transport, scope, and maximum age.
type Options struct {
	Source           string
	Destination      string
	Token            string
	Repositories     []string
	MaxAge           time.Duration
	MaxEnrichmentAge time.Duration
}

// Acquire verifies one resolved generation and installs a private local database.
func Acquire(ctx context.Context, o Options) (Manifest, error) {
	var m Manifest
	if o.MaxAge <= 0 || o.MaxEnrichmentAge < 0 || o.Destination == "" || o.Source == "" || len(o.Repositories) == 0 {
		return m, fmt.Errorf("source, destination, required repositories and positive maximum age are required")
	}
	remote := strings.HasPrefix(o.Source, "https://") || strings.HasPrefix(o.Source, "http://")
	var body []byte
	var err error
	if remote {
		body, err = downloadBytes(ctx, o.Source, o.Token, 1<<20)
	} else {
		body, err = os.ReadFile(o.Source)
	}
	if err != nil {
		return m, fmt.Errorf("resolve latest snapshot: %w", err)
	}
	m, err = decode(body)
	if err != nil {
		return m, fmt.Errorf("validate snapshot manifest: %w", err)
	}
	if err := validateAge(m.CollectedAt, o.MaxAge, "collection"); err != nil {
		return m, err
	}
	if o.MaxEnrichmentAge > 0 {
		if err := validateAge(m.EnrichedAt, o.MaxEnrichmentAge, "enrichment"); err != nil {
			return m, err
		}
	}
	if !sameScope(m.Repositories, o.Repositories) {
		return m, fmt.Errorf("snapshot repository scope does not match required scope")
	}
	dir := filepath.Dir(o.Destination)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return m, fmt.Errorf("create acquisition directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".acquire-*.sqlite")
	if err != nil {
		return m, fmt.Errorf("create private snapshot: %w", err)
	}
	temp := f.Name()
	defer remove(temp)
	var input io.ReadCloser
	if remote {
		u, parseErr := url.Parse(o.Source)
		if parseErr != nil {
			closeFile(f)
			return m, fmt.Errorf("parse manifest URL: %w", parseErr)
		}
		u.Path = path.Join(path.Dir(u.Path), m.Filename)
		u.RawQuery = ""
		input, err = download(ctx, u.String(), o.Token)
	} else {
		source := filepath.Join(filepath.Dir(o.Source), m.Filename)
		sourceAbs, absErr := filepath.Abs(source)
		if absErr != nil {
			closeFile(f)
			return m, fmt.Errorf("resolve snapshot path: %w", absErr)
		}
		destAbs, absErr := filepath.Abs(o.Destination)
		if absErr != nil {
			closeFile(f)
			return m, fmt.Errorf("resolve destination: %w", absErr)
		}
		if sourceAbs == destAbs {
			closeFile(f)
			return m, fmt.Errorf("destination must be a private copy")
		}
		input, err = os.Open(source)
	}
	if err != nil {
		closeFile(f)
		return m, fmt.Errorf("open pinned snapshot: %w", err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(input, maxSnapshot+1))
	inputErr := input.Close()
	if copyErr != nil {
		closeFile(f)
		return m, fmt.Errorf("copy pinned snapshot: %w", copyErr)
	}
	if inputErr != nil {
		closeFile(f)
		return m, fmt.Errorf("close pinned source: %w", inputErr)
	}
	if n > maxSnapshot {
		closeFile(f)
		return m, fmt.Errorf("snapshot exceeds 2 GiB")
	}
	if err := f.Sync(); err != nil {
		closeFile(f)
		return m, fmt.Errorf("fsync private snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		return m, fmt.Errorf("close private snapshot: %w", err)
	}
	if hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return m, fmt.Errorf("snapshot checksum mismatch")
	}
	s, err := inspect(ctx, temp)
	if err != nil {
		return m, fmt.Errorf("validate private snapshot: %w", err)
	}
	if s.SchemaVersion != m.SchemaVersion || s.Generation != m.Generation || s.CollectionVersion != m.CollectionVersion || s.CollectedAt != m.CollectedAt || s.EnrichedAt != m.EnrichedAt || !sameScope(s.Repositories, m.Repositories) {
		return m, fmt.Errorf("snapshot embedded metadata does not match manifest")
	}
	if m.EmbeddingFingerprint != "" && (s.Semantic == nil || s.Semantic.Fingerprint != m.EmbeddingFingerprint || s.Semantic.Dimension != m.EmbeddingDimension || s.Semantic.Generation != m.IndexGeneration) {
		return m, fmt.Errorf("snapshot embedding metadata does not match manifest")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(o.Destination + suffix); err == nil {
			return m, fmt.Errorf("destination has SQLite sidecars; choose a new private filename")
		} else if !os.IsNotExist(err) {
			return m, fmt.Errorf("inspect destination sidecar: %w", err)
		}
	}
	if err := os.Rename(temp, o.Destination); err != nil {
		return m, fmt.Errorf("install private snapshot: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return m, fmt.Errorf("persist private snapshot: %w", err)
	}
	return m, nil
}

func validateAge(timestamp string, maximum time.Duration, resource string) error {
	when, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return fmt.Errorf("parse snapshot %s time: %w", resource, err)
	}
	if time.Since(when) > maximum || time.Until(when) > 5*time.Minute {
		return fmt.Errorf("snapshot %s is stale or has a future timestamp", resource)
	}
	return nil
}
func sameScope(a, b []string) bool {
	left := append([]string{}, a...)
	right := append([]string{}, b...)
	sort.Strings(left)
	sort.Strings(right)
	return strings.Join(left, "\n") == strings.Join(right, "\n")
}
func download(ctx context.Context, target, token string) (io.ReadCloser, error) {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid snapshot URL")
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
		return nil, fmt.Errorf("snapshot HTTP allowed only on loopback")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("prepare snapshot download: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download snapshot: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if err := resp.Body.Close(); err != nil {
			slog.Log(ctx, diagnostics.TraceLevel, "close failed download", "error", err)
		}
		return nil, fmt.Errorf("snapshot endpoint returned HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}
func downloadBytes(ctx context.Context, target, token string, limit int64) ([]byte, error) {
	r, err := download(ctx, target, token)
	if err != nil {
		return nil, fmt.Errorf("download manifest: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(r, limit+1))
	closeErr := r.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read manifest response: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close manifest response: %w", closeErr)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("manifest response too large")
	}
	return body, nil
}

// ValidFilename reports whether an HTTP snapshot path is a single immutable filename.
func ValidFilename(name string) bool { return filenamePattern.MatchString(name) }
