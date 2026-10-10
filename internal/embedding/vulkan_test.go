package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestLoadedVulkan(t *testing.T) {
	good := "using device Vulkan0 (AMD Radeon 890M Graphics (RADV STRIX1)) (0000:c5:00.0)\noffloaded 7/7 layers to GPU\n"
	for _, c := range []struct {
		name, logs string
		ok         bool
	}{{"verified", good, true}, {"partial", strings.Replace(good, "7/7", "5/7", 1), false}, {"CPU", "CPU model buffer", false}, {"wrong device", strings.Replace(good, "Vulkan0", "Vulkan1", 1), false}} {
		t.Run(c.name, func(t *testing.T) {
			device, err := loadedVulkan(c.logs, "Vulkan0")
			if (err == nil) != c.ok {
				t.Fatal(device, err)
			}
			if c.ok && device != "Vulkan0: AMD Radeon 890M Graphics (RADV STRIX1)" {
				t.Fatal("device truncated", device)
			}
		})
	}
}

func TestVulkanCompatibilityGuard(t *testing.T) {
	raw, err := references.ReadFile("testdata/minilm-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var refs struct {
		Cases []struct {
			IDs    []int64   `json:"input_ids"`
			Vector []float32 `json:"embedding"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("software ", MaxTokens-2)
	longIDs, err := Default.Tokens(long)
	if err != nil {
		t.Fatal(err)
	}
	longVector, err := Default.Embed(context.Background(), long)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "wrong weights or pooling"}[bad], func(t *testing.T) {
			p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					Input [][]int64 `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Error(err)
					return
				}
				out := make([][]float32, len(in.Input))
				for n, ids := range in.Input {
					if equalIDs(ids, longIDs) {
						out[n] = longVector
					}
					for _, ref := range refs.Cases {
						if equalIDs(ids, ref.IDs) {
							out[n] = ref.Vector
						}
					}
					if bad {
						out[n] = make([]float32, Dimension)
						out[n][0] = 1
					}
				}
				writeVectors(t, w, runtimeModel, out)
			}, 16)
			gpu, err := p.start(context.Background(), p.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer gpu.close()
			if err := gpu.verify(context.Background()); (err == nil) == bad {
				t.Fatal("compatibility guard", err)
			}
		})
	}
}

func TestVulkanEnvironment(t *testing.T) {
	t.Setenv("LLAMA_ARG_HF_REPO", "another/model")
	t.Setenv("LLAMA_ARG_MODEL", "wrong.gguf")
	t.Setenv("GGML_RPC_SERVERS", "remote")
	t.Setenv("GGML_VK_DISABLE_F16", "0")
	env := strings.Join(vulkanEnvironment(), "\n")
	for _, value := range []string{"another/model", "wrong.gguf", "GGML_RPC_SERVERS=", "GGML_VK_DISABLE_F16=0"} {
		if strings.Contains(env, value) {
			t.Fatal("inherited inference override", value)
		}
	}
	if !strings.Contains(env, "GGML_VK_DISABLE_F16=1") {
		t.Fatal("FP32 shader mode missing")
	}
}
