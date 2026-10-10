package embedding

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GGUF exports the bundled FP32 tensors to a verified local llama.cpp model file.
// It performs no downloads and preserves the embedding compatibility identity.
func GGUF() (string, error) {
	dir, err := extract()
	if err != nil {
		return "", err
	}
	raw, err := assets.ReadFile("assets/model.safetensors")
	if err != nil {
		return "", fmt.Errorf("read bundled tensors: %w", err)
	}
	vocab, err := assets.ReadFile("assets/vocab.txt")
	if err != nil {
		return "", fmt.Errorf("read bundled vocabulary: %w", err)
	}
	data, err := ggufModel(raw, string(vocab))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	path := filepath.Join(dir, "minilm-fp32-v1.gguf")
	valid, err := fileMatches(path, hex.EncodeToString(digest[:]), int64(len(data)))
	if err != nil {
		return "", err
	}
	if valid {
		return path, nil
	}
	f, err := os.CreateTemp(dir, ".gguf-*")
	if err != nil {
		return "", fmt.Errorf("create GGUF: %w", err)
	}
	defer func() {
		if err := os.Remove(f.Name()); err != nil && !os.IsNotExist(err) {
			slog.Debug("remove temporary GGUF", "error", err)
		}
	}()
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return "", fmt.Errorf("write GGUF: %w", writeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close GGUF: %w", closeErr)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return "", fmt.Errorf("install GGUF: %w", err)
	}
	return path, nil
}

type safeTensor struct {
	Dtype   string    `json:"dtype"`
	Shape   []uint64  `json:"shape"`
	Offsets [2]uint64 `json:"data_offsets"`
}

type ggufWriter struct {
	bytes.Buffer
	err error
}

func (w *ggufWriter) number(v any) {
	if w.err == nil {
		w.err = binary.Write(&w.Buffer, binary.LittleEndian, v)
	}
}
func (w *ggufWriter) str(s string) { w.number(uint64(len(s))); w.WriteString(s) }
func (w *ggufWriter) value(v any) {
	switch v := v.(type) {
	case string:
		w.number(uint32(8))
		w.str(v)
	case uint32:
		w.number(uint32(4))
		w.number(v)
	case float32:
		w.number(uint32(6))
		w.number(math.Float32bits(v))
	case bool:
		w.number(uint32(7))
		w.number(v)
	case []string:
		w.number(uint32(9))
		w.number(uint32(8))
		w.number(uint64(len(v)))
		for _, x := range v {
			w.str(x)
		}
	case []int32:
		w.number(uint32(9))
		w.number(uint32(5))
		w.number(uint64(len(v)))
		for _, x := range v {
			w.number(x)
		}
	default:
		w.err = fmt.Errorf("unsupported GGUF metadata %T", v)
	}
}
func (w *ggufWriter) align() {
	for w.Len()%32 != 0 {
		w.WriteByte(0)
	}
}

func ggufModel(raw []byte, vocab string) ([]byte, error) {
	if len(raw) < 8 {
		return nil, fmt.Errorf("invalid bundled safetensors header")
	}
	h := binary.LittleEndian.Uint64(raw[:8])
	if h > uint64(len(raw)-8) {
		return nil, fmt.Errorf("invalid bundled safetensors length")
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw[8:8+h], &entries); err != nil {
		return nil, fmt.Errorf("decode tensor header: %w", err)
	}
	tensors := map[string]safeTensor{}
	for name, entry := range entries {
		if name == "__metadata__" || name == "embeddings.position_ids" || strings.HasPrefix(name, "pooler.") {
			continue
		}
		var t safeTensor
		if err := json.Unmarshal(entry, &t); err != nil {
			return nil, fmt.Errorf("decode tensor %s: %w", name, err)
		}
		mapped, err := ggufTensor(name)
		if err != nil {
			return nil, err
		}
		count := uint64(1)
		for _, d := range t.Shape {
			if d == 0 || count > uint64(len(raw))/d {
				return nil, fmt.Errorf("invalid tensor shape %s", name)
			}
			count *= d
		}
		if t.Dtype != "F32" || len(t.Shape) < 1 || len(t.Shape) > 2 || t.Offsets[1] < t.Offsets[0] || t.Offsets[1] > uint64(len(raw))-8-h || t.Offsets[1]-t.Offsets[0] != count*4 {
			return nil, fmt.Errorf("invalid FP32 tensor %s", name)
		}
		tensors[mapped] = t
	}
	if len(tensors) != 101 {
		return nil, fmt.Errorf("expected 101 MiniLM tensors, got %d", len(tensors))
	}
	tokens := strings.Split(strings.TrimSuffix(vocab, "\n"), "\n")
	if len(tokens) != 30522 {
		return nil, fmt.Errorf("invalid MiniLM vocabulary size %d", len(tokens))
	}
	types := make([]int32, len(tokens))
	for n, tok := range tokens {
		types[n] = 1
		if n == 0 || n >= 100 && n <= 103 {
			types[n] = 3
			if n == 100 {
				types[n] = 2
			}
			continue
		}
		if strings.HasPrefix(tok, "##") {
			tokens[n] = tok[2:]
		} else {
			tokens[n] = "▁" + tok
		}
	}
	meta := map[string]any{
		"general.architecture": "bert", "general.name": Model, "general.file_type": uint32(0), "general.alignment": uint32(32), "gh-mirror.fingerprint": Fingerprint,
		"bert.context_length": uint32(512), "bert.embedding_length": uint32(Dimension), "bert.block_count": uint32(6), "bert.feed_forward_length": uint32(1536), "bert.attention.head_count": uint32(12), "bert.attention.layer_norm_epsilon": float32(1e-12), "bert.attention.causal": false, "bert.pooling_type": uint32(1), "tokenizer.ggml.token_type_count": uint32(2),
		"tokenizer.ggml.model": "bert", "tokenizer.ggml.pre": "default", "tokenizer.ggml.tokens": tokens, "tokenizer.ggml.token_type": types, "tokenizer.ggml.bos_token_id": uint32(101), "tokenizer.ggml.eos_token_id": uint32(102), "tokenizer.ggml.unknown_token_id": uint32(100), "tokenizer.ggml.padding_token_id": uint32(0), "tokenizer.ggml.separator_token_id": uint32(102), "tokenizer.ggml.add_bos_token": true, "tokenizer.ggml.add_eos_token": true,
	}
	w := &ggufWriter{}
	w.WriteString("GGUF")
	w.number(uint32(3))
	w.number(uint64(len(tensors)))
	w.number(uint64(len(meta)))
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w.str(k)
		w.value(meta[k])
	}
	names := make([]string, 0, len(tensors))
	for k := range tensors {
		names = append(names, k)
	}
	sort.Strings(names)
	offset := uint64(0)
	for _, name := range names {
		t := tensors[name]
		w.str(name)
		w.number(uint32(len(t.Shape)))
		for n := len(t.Shape) - 1; n >= 0; n-- {
			w.number(t.Shape[n])
		}
		w.number(uint32(0))
		w.number(offset)
		offset = (offset + t.Offsets[1] - t.Offsets[0] + 31) / 32 * 32
	}
	w.align()
	for _, name := range names {
		t := tensors[name]
		w.Write(raw[8+h+t.Offsets[0] : 8+h+t.Offsets[1]])
		w.align()
	}
	if w.err != nil {
		return nil, fmt.Errorf("encode GGUF: %w", w.err)
	}
	return w.Bytes(), nil
}

func ggufTensor(name string) (string, error) {
	base := map[string]string{"embeddings.word_embeddings": "token_embd", "embeddings.position_embeddings": "position_embd", "embeddings.token_type_embeddings": "token_types", "embeddings.LayerNorm": "token_embd_norm"}
	for old, new := range base {
		if strings.HasPrefix(name, old+".") {
			return new + strings.TrimPrefix(name, old), nil
		}
	}
	for n := 0; n < 6; n++ {
		prefix := fmt.Sprintf("encoder.layer.%d.", n)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		part := strings.TrimPrefix(name, prefix)
		layers := map[string]string{"attention.self.query": "attn_q", "attention.self.key": "attn_k", "attention.self.value": "attn_v", "attention.output.dense": "attn_output", "attention.output.LayerNorm": "attn_output_norm", "intermediate.dense": "ffn_up", "output.dense": "ffn_down", "output.LayerNorm": "layer_output_norm"}
		for old, new := range layers {
			if strings.HasPrefix(part, old+".") {
				return fmt.Sprintf("blk.%d.%s%s", n, new, strings.TrimPrefix(part, old)), nil
			}
		}
	}
	return "", fmt.Errorf("unsupported bundled tensor %s", name)
}
