package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestReferenceVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/minilm-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Cases []struct {
			Text   string    `json:"text"`
			IDs    []int64   `json:"input_ids"`
			Vector []float32 `json:"embedding"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	for _, c := range golden.Cases {
		t.Run(c.Text, func(t *testing.T) {
			ids, err := Default.Tokens(c.Text)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(ids, c.IDs) {
				t.Fatalf("token mismatch: got %v want %v", ids, c.IDs)
			}
			v, err := Default.Embed(context.Background(), c.Text)
			if err != nil {
				t.Fatal(err)
			}
			var dot, norm, refnorm float64
			for n, x := range v {
				dot += float64(x) * float64(c.Vector[n])
				norm += float64(x) * float64(x)
				refnorm += float64(c.Vector[n]) * float64(c.Vector[n])
			}
			cosine := dot / math.Sqrt(norm*refnorm)
			if cosine < 0.99999 {
				t.Fatalf("reference cosine %.9f", cosine)
			}
		})
	}
}
func TestChunking(t *testing.T) {
	for _, text := range []string{"", strings.Repeat("Detailed technical report with Unicode café 日本語! ", 300), strings.Repeat("界", 1400), strings.Repeat("x", 3000)} {
		t.Run(string([]rune(text)[:min(16, len([]rune(text)))]), func(t *testing.T) {
			chunks, err := Default.Chunks(text)
			if err != nil {
				t.Fatal(err)
			}
			end := 0
			for _, c := range chunks {
				if len([]rune(c.Text)) > maxChunkRunes {
					t.Fatal("chunk exceeds character bound")
				}
				if c.Start > end || c.End <= c.Start || c.Text != text[c.Start:c.End] {
					t.Fatal("chunk lost or modified input", c.Start, c.End, end)
				}
				ids, err := Default.Tokens(c.Text)
				if err != nil {
					t.Fatal(err)
				}
				if len(ids) > MaxTokens {
					t.Fatal("chunk exceeds token budget", len(ids))
				}
				end = c.End
			}
			if end != len(text) {
				t.Fatal("tail lost", end, len(text))
			}
		})
	}
}
func TestOfflineCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Default.Embed(ctx, "sample"); err == nil {
		t.Fatal("cancelled inference accepted")
	}
	if _, err := Default.Embed(context.Background(), strings.Repeat("word ", 400)); err == nil {
		t.Fatal("oversized passage truncated")
	}
	license, err := License()
	if err != nil || !strings.Contains(license, "Apache") {
		t.Fatal("missing bundled license", err)
	}
}
func BenchmarkMiniLM(b *testing.B) {
	for _, tokens := range []int{16, 64, 200} {
		text := strings.Repeat("software ", tokens)
		b.Run(fmt.Sprintf("tokens_%d", tokens), func(b *testing.B) {
			if _, err := Default.Embed(context.Background(), text); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				if _, err := Default.Embed(context.Background(), text); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type rejectNetwork struct{ calls int }

func (r *rejectNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, fmt.Errorf("unexpected network request")
}
func TestBundledLoadWithoutNetwork(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	old := http.DefaultTransport
	blocked := &rejectNetwork{}
	http.DefaultTransport = blocked
	t.Cleanup(func() { http.DefaultTransport = old })
	encoder := &Encoder{}
	v, err := encoder.Embed(context.Background(), "all weights and tokenizer are bundled locally")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != Dimension || blocked.calls != 0 {
		t.Fatal("offline encoder attempted network access", blocked.calls)
	}
}
