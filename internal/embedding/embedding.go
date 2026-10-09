// Package embedding provides the bundled, offline MiniLM encoder and chunking contract.
package embedding

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/rostamlabs/rembed"
	"github.com/rostamlabs/rembed/tokenizer"
)

// Model is the bundled, Apache-2.0 licensed sentence encoder.
const Model = "sentence-transformers/all-MiniLM-L6-v2"

// Revision pins both weights and tokenizer to an immutable upstream revision.
const Revision = "1110a243fdf4706b3f48f1d95db1a4f5529b4d41"

// Dimension is the size of each normalized embedding.
const Dimension = 384

// MaxTokens includes the two WordPiece framing tokens.
const MaxTokens = 256

// Fingerprint identifies weights, tokenizer, pooling, precision and chunking together.
const Fingerprint = "minilm-1110a243-rembed-19d673b-fp32-mean-l2-chunk256-2048r-overlap32r-v1"

const maxChunkRunes = 2048

//go:embed assets
var assets embed.FS

// Chunk preserves an excerpt and byte offsets into its original text.
type Chunk struct {
	Text       string
	Start, End int
}

// Encoder lazily loads one CPU encoder; concurrent calls share immutable weights.
type Encoder struct {
	once  sync.Once
	model *rembed.Embedder
	tok   *tokenizer.Tokenizer
	err   error
}

// Default is the process-wide encoder; it never contacts a model service.
var Default = &Encoder{}

func (e *Encoder) load() error {
	e.once.Do(func() {
		var dir string
		dir, e.err = extract()
		if e.err != nil {
			return
		}
		e.tok, e.err = tokenizer.New(filepath.Join(dir, "vocab.txt"), true, "[CLS]", "[SEP]", "[UNK]")
		if e.err != nil {
			return
		}
		// One thread per inference avoids nested spinning pools when indexing texts concurrently.
		e.model, e.err = rembed.Load(dir, rembed.WithWorkers(1))
	})
	if e.err != nil {
		return fmt.Errorf("load bundled MiniLM: %w", e.err)
	}
	return nil
}

// ID returns the complete embedding compatibility contract.
func (e *Encoder) ID() string { return Fingerprint }

// Dim returns the number of float32 coordinates.
func (e *Encoder) Dim() int { return Dimension }

// Tokens returns untruncated WordPiece IDs, including framing tokens.
func (e *Encoder) Tokens(text string) ([]int64, error) {
	if err := e.load(); err != nil {
		return nil, err
	}
	ids, _ := e.tok.Encode(text, len(text)*4+2)
	return ids, nil
}

// Embed encodes one bounded passage without silently discarding its tail.
func (e *Encoder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("encode passage: %w", err)
	}
	ids, err := e.Tokens(text)
	if err != nil {
		return nil, err
	}
	if len(ids) > MaxTokens {
		return nil, fmt.Errorf("passage has %d tokens; maximum is %d", len(ids), MaxTokens)
	}
	vectors, err := e.model.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("encode MiniLM passage: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("finish passage: %w", err)
	}
	return vectors[0], nil
}

// Chunks splits all input into overlapping bounded passages without losing Unicode or tails.
func (e *Encoder) Chunks(text string) ([]Chunk, error) {
	if err := e.load(); err != nil {
		return nil, err
	}
	offsets := []int{}
	for offset := range text {
		offsets = append(offsets, offset)
	}
	offsets = append(offsets, len(text))
	chunks := []Chunk{}
	for start := 0; start < len(offsets)-1; {
		low, high := start+1, min(start+maxChunkRunes, len(offsets)-1)
		for low < high {
			mid := (low + high + 1) / 2
			ids, _ := e.tok.Encode(text[offsets[start]:offsets[mid]], MaxTokens+1)
			if len(ids) <= MaxTokens {
				low = mid
			} else {
				high = mid - 1
			}
		}
		end := low
		if end < len(offsets)-1 {
			for n := end; n > start+(end-start)*3/4; n-- {
				r := []rune(text[offsets[n-1]:offsets[n]])[0]
				if unicode.IsSpace(r) {
					end = n
					break
				}
			}
		}
		passage := text[offsets[start]:offsets[end]]
		if strings.TrimSpace(passage) != "" {
			chunks = append(chunks, Chunk{passage, offsets[start], offsets[end]})
		}
		if end == len(offsets)-1 {
			break
		}
		next := end - 32
		if next <= start {
			next = end
		}
		start = next
	}
	return chunks, nil
}

type provenance struct {
	Files map[string]struct {
		SHA256 string `json:"sha256"`
		Bytes  int64  `json:"bytes"`
	} `json:"files"`
}

func extract() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve model cache: %w", err)
	}
	dir, err := filepath.Abs(filepath.Join(cache, "gh-mirror", "models", Revision))
	if err != nil {
		return "", fmt.Errorf("resolve model directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create model cache: %w", err)
	}
	raw, err := assets.ReadFile("assets/provenance.json")
	if err != nil {
		return "", fmt.Errorf("read model provenance: %w", err)
	}
	var manifest provenance
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", fmt.Errorf("decode model provenance: %w", err)
	}
	for name, expected := range manifest.Files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		valid, err := fileMatches(path, expected.SHA256, expected.Bytes)
		if err != nil {
			return "", err
		}
		if valid {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", fmt.Errorf("create model asset directory: %w", err)
		}
		if err := writeAsset(path, "assets/"+name, expected.SHA256); err != nil {
			return "", err
		}
	}
	// An absolute existing path cannot be interpreted by rembed as a Hub model ID.
	return dir, nil
}
func fileMatches(path, digest string, size int64) (bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open model asset: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("close model asset", "error", err)
		}
	}()
	stat, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat model asset: %w", err)
	}
	if stat.Size() != size {
		return false, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("verify model asset: %w", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)) == digest, nil
}
func writeAsset(path, name, digest string) (err error) {
	src, err := assets.Open(name)
	if err != nil {
		return fmt.Errorf("open bundled asset: %w", err)
	}
	defer func() { err = joinClose(err, src.Close()) }()
	dst, err := os.CreateTemp(filepath.Dir(path), ".asset-*")
	if err != nil {
		return fmt.Errorf("stage model asset: %w", err)
	}
	temp := dst.Name()
	defer func() {
		if removeErr := os.Remove(temp); removeErr != nil && !os.IsNotExist(removeErr) {
			err = joinClose(err, removeErr)
		}
	}()
	h := sha256.New()
	if _, copyErr := io.Copy(io.MultiWriter(dst, h), src); copyErr != nil {
		return joinClose(fmt.Errorf("extract model asset: %w", copyErr), dst.Close())
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != digest {
		return joinClose(fmt.Errorf("bundled model asset checksum mismatch: %s", name), dst.Close())
	}
	if syncErr := dst.Sync(); syncErr != nil {
		return joinClose(syncErr, dst.Close())
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close extracted asset: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("install model asset: %w", err)
	}
	return nil
}
func joinClose(prior, err error) error {
	if err != nil {
		if prior != nil {
			return fmt.Errorf("%w; close asset: %v", prior, err)
		}
		return fmt.Errorf("close asset: %w", err)
	}
	return prior
}

// License returns the bundled model's Apache-2.0 license text.
func License() (string, error) {
	raw, err := fs.ReadFile(assets, "assets/LICENSE")
	if err != nil {
		return "", fmt.Errorf("read model license: %w", err)
	}
	return string(raw), nil
}
