# Vulkan embeddings benchmark, 2026-10-10

Vulkan improved indexing throughput by 2.30× with eight document workers and
1.80× with sixteen on this laptop. An eight-passage batch performed best among
the tested settings and is now the default. Whole-search latency stayed similar
because local database work dominates inference.

Measurements used an AMD Ryzen AI 9 HX PRO 370, twelve physical cores, twenty-four
logical CPUs, approximately 64 GiB RAM, and a Radeon 890M running RADV STRIX1.
The toolchain was Go 1.26.8 with the nodwarf5 experiment; Lemonade was 2026.41.1
and its installed llama.cpp Vulkan runtime was b10825. The implementation was
`a727382`, with explicit batch settings; subsequent default tuning and cursor
binding do not change embedding inference. Runs were sequential, with a warm
filesystem/model cache and no concurrent validation or collection jobs. Three
repetitions provide a local comparison, not a sustained load or thermal test.

Both paths used the bundled MiniLM L6 revision
`1110a243fdf4706b3f48f1d95db1a4f5529b4d41`: identical FP32 tensors, bundled
WordPiece token IDs, mean pooling, L2 normalization and chunking. The Go exporter
produced an 86.7 MiB GGUF with SHA-256
`f99bb24e9f8867682576da8553f7b716260d91d1f28d26967c248b1cf0fea7fe`.
The Vulkan runtime confirmed `Vulkan0: AMD Radeon 890M Graphics (RADV STRIX1)` and
7/7 layers offloaded. FP16 shaders and flash attention were disabled.

The runtime is the llama.cpp backend installed by Lemonade, launched directly as
a dedicated authenticated loopback process. It receives integer token arrays and
runs with `--offline`, using a local model file. It does not call the Lemonade
daemon or download a second model. See the upstream
[Lemonade backend reference](https://github.com/lemonade-sdk/lemonade/blob/main/docs/dev/backends-reference.md)
and [llama.cpp server reference](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md).

## Indexing

The same 256 uniformly sampled passages used in the earlier benchmark produced
258 bounded passages after local chunking. Each passage became a separate body
document in a fresh temporary SQLite database. Creation, model startup and warmup
were excluded from indexing timings. Timings include preparation, inference,
FULL synchronous vector checkpoints and completion metadata. This small fixture
does not reproduce the distribution of multi-passage tickets in a full mirror.

| Backend | Document workers | Maximum batch | Median time | Vectors/s |
| --- | ---: | ---: | ---: | ---: |
| Go CPU | 8 | 1 | 2.654 s | 97.2 |
| Go CPU | 16 | 1 | 1.787 s | 144.3 |
| Vulkan | 8 | 8 | 1.152 s | 224.0 |
| Vulkan | 8 | 16 | 1.265 s | 204.0 |
| Vulkan | 16 | 8 | 0.993 s | 259.8 |
| Vulkan | 16 | 16 | 1.146 s | 225.2 |
| Vulkan | 16 | 32 | 1.206 s | 214.0 |

All twenty-one fresh indexing runs produced 258 vectors without SQLite busy
errors or CPU fallback. Repeating each unchanged index produced zero new
embeddings and took approximately 1.4–2.3 ms. Actual batches can be smaller than
the configured maximum, depending on available work and the tail of the queue.

These rates suggest approximately nine minutes for 119,582 passages at eight
GPU document workers, or eight minutes at sixteen. Those are extrapolations,
not measurements of a complete mirror rebuild; document structure, persistence,
other workloads and thermal behavior can change the result.

## Embedding and search latency

Twenty warm single-passage calls per length used repeated generic words.
Reported lengths include the two framing tokens. Vulkan times include token
preparation, the batching delay, localhost HTTP and response validation.

| Tokens | CPU median | Vulkan median |
| ---: | ---: | ---: |
| 18 | 10.70 ms | 6.09 ms |
| 66 | 35.73 ms | 7.76 ms |
| 202 | 138.11 ms | 9.95 ms |

Whole-search timings used an immutable 700.3 MiB snapshot with 119,582 vector
passages, narrowed to one repository containing 9,555 pull requests. Four generic
queries, including a technical identifier, ran three times per engine/backend.
Each query requested twenty summary results with one evidence excerpt. These
measurements used a sixteen-passage GPU maximum. The p95 for twelve samples is
the largest observation under the nearest-rank definition and is imprecise.

| Engine | CPU median / p95 | Vulkan median / p95 |
| --- | ---: | ---: |
| Semantic | 2.133 / 2.635 s | 2.163 / 2.900 s |
| Hybrid | 2.457 / 2.731 s | 2.466 / 3.545 s |

GPU inference alone is faster, but these end-to-end searches show no meaningful
latency improvement. The [earlier CPU profile](benchmark-2026-10-10.md) attributed
most query time to SQLite execution/decoding. Reducing that work is the next
query-performance opportunity.

Three fresh `model --check` processes per backend measured median startup,
verification and first inference at 356 ms on CPU and 1,730 ms on Vulkan with
the new eight-passage default. The GPU process is reused for a long-running
service or indexing command; short CLI searches pay startup again. CPU remains
a useful choice for occasional CLI queries.

## Compatibility and interruption

Before serving vectors, the GPU provider checked eleven attributed references,
the bundled tokenizer IDs and a full 256-token passage. A separate comparison
of all 258 sampled passages found minimum CPU/GPU cosine similarity
**0.99999619** and maximum coordinate difference **0.00051723**. Runtime arithmetic
differs, so results are compatible within the documented tolerance rather than
bit-identical. No model was changed or quantized.

Across the twenty-four search comparisons, top-twenty set overlap was 98.75%
(474/480 results); fifteen pages had exactly the same ordering. This measures
agreement rather than relevance accuracy: the generic queries
have no reviewed judgments. Small numerical differences can affect near ties.
Semantic cursors now bind the actual query vector, returning a stale-cursor error
when backend arithmetic changes instead of continuing a changed ranking.

A disposable hard-stop test retained 152 completed vectors; restarting computed
the remaining 106. A separate SIGINT test retained 67 and resumed the remaining
191. Both finished with all 258 vectors, using the same model and local documents.
Unit tests also cover interrupted multi-passage batches, changed documents,
unchanged indexing, malformed responses, missing runtimes and same-model fallback.

No GitHub collection or remote model calls were made. The source snapshot was
opened read-only; only temporary fixture databases were indexed. Private texts,
ticket identities, raw rankings and logs remain in ignored local benchmark
artifacts and are excluded from this report.

For this hardware, start with:

```sh
gh-mirror model --check --embedding-backend lemonade-vulkan --format text
gh-mirror index --workers 8 --embedding-backend lemonade-vulkan
```

Sixteen workers gave the highest measured throughput if the additional document
preparation concurrency is useful. Keep `--embedding-backend cpu` available for
standalone deployments and ad-hoc queries.
