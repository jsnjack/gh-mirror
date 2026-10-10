package embedding

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
)

func TestGGUF(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path, err := GGUF()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Pins every metadata field, tensor shape, offset, tokenizer entry and FP32 byte.
	const expected = "f99bb24e9f8867682576da8553f7b716260d91d1f28d26967c248b1cf0fea7fe"
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != expected {
		t.Fatalf("GGUF checksum %s", got)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := GGUF()
	if err != nil {
		t.Fatal(err)
	}
	valid, err := fileMatches(again, expected, int64(len(raw)))
	if err != nil || !valid || again != path {
		t.Fatalf("corruption recovery %s %v %v", again, valid, err)
	}
}

func TestGGUFInvalidTensors(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("invalid header"), {2, 0, 0, 0, 0, 0, 0, 0, '{', '}'}} {
		t.Run(fmt.Sprintf("size_%d", len(raw)), func(t *testing.T) {
			if _, err := ggufModel(raw, ""); err == nil {
				t.Fatal("invalid tensors accepted")
			}
		})
	}
}

func TestGGUFTensorNames(t *testing.T) {
	for _, c := range []struct{ in, out string }{{"embeddings.word_embeddings.weight", "token_embd.weight"}, {"encoder.layer.5.attention.output.LayerNorm.bias", "blk.5.attn_output_norm.bias"}, {"encoder.layer.0.output.LayerNorm.weight", "blk.0.layer_output_norm.weight"}} {
		t.Run(c.in, func(t *testing.T) {
			got, err := ggufTensor(c.in)
			if err != nil || got != c.out {
				t.Fatalf("mapping %s %v", got, err)
			}
		})
	}
	if _, err := ggufTensor("encoder.layer.6.output.dense.weight"); err == nil {
		t.Fatal("unknown tensor accepted")
	}
}
