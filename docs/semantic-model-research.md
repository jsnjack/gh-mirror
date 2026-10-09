# Offline embedding model research

Research date: 2026-10-09. The deployment target is one portable Go executable and
SQLite snapshots for personal use and small organizations. Retrieval includes
natural-language questions, duplicate candidates, technical identifiers and client
names stored in labels. No model service or runtime download is required.

## Recommendation

Bundle `sentence-transformers/all-MiniLM-L6-v2` as requested. It is a defensible
baseline for general sentence similarity and duplicate discovery, and has reference
vectors that can validate the pure Go implementation. The user's acceptance of a
roughly 90 MB model is **not a maximum size requirement**. Larger models belong in
this comparison; the actual constraints are portable offline deployment, CPU
throughput and retrieval quality.
Use hybrid search for everyday ticket queries so literal identifiers and labels
remain useful alongside paraphrases. This recommendation is an engineering choice,
not a claim that MiniLM leads retrieval benchmarks.

For a replacement, evaluate three distinct options: **Arctic Embed XS** for similar
CPU cost, **BGE Base EN v1.5** for a larger English retrieval encoder that already
passes the Go engine's reference checks, and **Granite Small English R2** for a
newer English encoder with technical-domain training and longer context. Granite
requires inference compatibility work before a trustworthy local comparison.
For multilingual needs, include **Qwen3 Embedding 0.6B**; for longer English
documents, include **Nomic Embed Text v1.5**. No general benchmark establishes the
best model for these tickets; choose using held-out ticket relevance judgments and measured CPU
latency. RAM capacity alone does not decide the winner.

A replacement needs its prescribed pooling, query/document instructions, a new
fingerprint and local vector reindexing. Different dimensions also require a
vector-schema migration: the shipped schema explicitly stores 384 coordinates.
GitHub collection does not need rebuilding. The bundled model remains fixed;
there is no runtime model selector or automatic download.

## Hardware measured locally

The machine has an AMD Ryzen AI 9 HX PRO 370, 12 physical cores, 24 hardware threads
and approximately 64 GB of installed RAM. Its CPU supports AVX2/FMA and additional
AVX-512/VNNI instructions. The implementation uses ordinary CPU inference; neither
Radeon graphics nor the NPU requires drivers or participates in inference. Model
memory is small relative to this machine, so compatibility and CPU throughput
matter more than fitting the weights into RAM.

Four independent inference workers are the portable default. Eight or sixteen can
be useful on this machine; those settings should be measured on the actual corpus
and chosen according to other workloads. A small CI runner can use one worker.
Inference uses FP32 and one internal engine worker per passage to avoid nested
thread pools and spinning workers.

## Models compared

Asset sizes below refer to the actual downloaded safetensors file, where measured.
Parameter counts, context limits and evaluation scores are from the linked primary
model cards. Published benchmark numbers are self-reported and are not measurements
of GitHub duplicate accuracy. Legacy MTEB, MTEB English v2, multilingual MTEB and
BEIR evaluation cohorts differ; do not sort their scores into one leaderboard.

| Model | Artifact / dimensions | Strength | Cost or limitation |
| --- | --- | --- | --- |
| [all-MiniLM-L6-v2](https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2) | 90,868,376 bytes FP32; 384 | Six-layer sentence encoder, mean pooling, general similarity; selected baseline | English-oriented; use its 256-token sentence-transformer limit rather than the 512-row position table |
| [Arctic Embed XS](https://huggingface.co/Snowflake/snowflake-arctic-embed-xs) | 90,272,656 bytes FP32; 384 | MiniLM-derived retrieval model; published retrieval NDCG@10 50.15 versus MiniLM 41.95 | CLS pooling and query instruction; benchmark advantage needs confirmation on tickets |
| [GTE Small](https://huggingface.co/thenlper/gte-small) | 66,746,168 bytes F16 asset; 384 | Smaller distributed asset; 512-token context; published retrieval score 49.46 | 33.4M parameters; expands into FP32 inference weights and was slower locally |
| [Multi-QA MiniLM L6](https://huggingface.co/sentence-transformers/multi-qa-MiniLM-L6-cos-v1) | Small six-layer MiniLM; 384 | Question-to-passage retrieval training on 215M question/answer pairs | Asymmetric retrieval training is not itself evidence of better duplicate detection; not benchmarked locally |
| [BGE Small EN v1.5](https://huggingface.co/BAAI/bge-small-en-v1.5) | About 133 MB FP32; 384 | Strong compact English retrieval model; published retrieval score 51.68; MIT license | Twelve layers and more CPU work; retrieval instruction recommended |
| [E5 Small v2](https://huggingface.co/intfloat/e5-small-v2) | About 133 MB FP32; 384 | Compact retrieval model; 512-token input | Requires query/passage prefixes; English; more compute than six-layer MiniLM |
| [GIST MiniLM L6](https://huggingface.co/avsolatorio/GIST-all-MiniLM-L6-v2) | MiniLM-sized; 384 | Same basic backbone with guided contrastive training | Another candidate for a judged comparison; Arctic's card reports a stronger retrieval result |
| [BGE Base EN v1.5](https://huggingface.co/BAAI/bge-base-en-v1.5) | 437,955,512 bytes FP32; 768 | English retrieval; published legacy retrieval score 53.25; MIT; verified Go reference vectors | About 4.5 times MiniLM's CPU latency at 64 tokens locally; twice the vector storage |
| [Granite Small English R2](https://huggingface.co/ibm-granite/granite-embedding-small-english-r2) | 95,332,048 bytes BF16 asset; 47M parameters; 384 | ModernBERT, 8,192-token context, technical training data; Apache-2.0 | Pinned Go loader rejects its position configuration; no validated local latency measurement |
| [Granite 30M English](https://huggingface.co/ibm-granite/granite-embedding-30m-english) | 60,602,024 bytes F16 asset; 384 | Smaller predecessor, permissive licensing; another compact alternative | Superseded by R2; not locally benchmarked; distributed precision differs from FP32 execution |
| [All MPNet Base v2](https://huggingface.co/sentence-transformers/all-mpnet-base-v2) | About 109M parameters; 768 | General sentence similarity alternative to MiniLM; engine has MPNet reference fixtures | Larger CPU and storage cost; similarity strength need not imply the best ticket retrieval |
| [Nomic Embed Text v1.5](https://huggingface.co/nomic-ai/nomic-embed-text-v1.5) | About 137M parameters; 768 with shortened-vector options | Long context and Matryoshka embeddings; Apache-2.0; engine has Nomic reference fixtures | Requires task prefixes and prescribed normalization; long-context configuration needs separate validation |
| [BGE M3](https://huggingface.co/BAAI/bge-m3) | About 0.6B parameters; 1,024 | Multilingual, 8,192-token context, dense/sparse/multi-vector retrieval | Substantially larger CPU workload; dense-only integration would not deliver all three retrieval modes |
| [EmbeddingGemma 300M](https://huggingface.co/google/embeddinggemma-300m) | 300M parameters | Multilingual, configurable shortened embeddings and longer context | Larger inference footprint and Gemma access/license terms; Go compatibility must be validated |
| [Qwen3 Embedding 0.6B](https://huggingface.co/Qwen/Qwen3-Embedding-0.6B) | 0.6B parameters | Multilingual retrieval with larger context and configurable dimensions | Much larger weights and CPU cost; RAM availability does not remove indexing throughput cost |

Granite reports BEIR-15 retrieval 50.9 and MTEB English v2 retrieval 53.9. Those
numbers use different evaluation cohorts from the legacy scores above. Its reported
GPU throughput is measured on an H100 and does not predict this Ryzen's CPU speed.
Qwen's larger 4B/8B variants are also relevant when quality takes priority over CPU
latency; they deserve a separate throughput evaluation rather than exclusion by
an invented artifact-size limit. None is an automatically superior choice for a
personal mirror with frequent updates.

GTE's smaller F16 file is not a promise of a smaller FP32 working set. Quantized
MiniLM can reduce computation and memory traffic, but changes numerical behavior.
Weight-only INT8 and full INT8 are different tradeoffs. The first shipped contract
uses FP32 on every architecture; future quantization needs reference checks and a
new index fingerprint rather than mixing vector contracts silently.

## Pure Go inference options

[rembed](https://github.com/rostamlabs/rembed) reads safetensors directly and provides
Go WordPiece tokenization, transformer operations and CPU assembly implementations
for amd64 and arm64. It avoids CGO, ONNX Runtime and shared-library packaging. The
chosen dependency is pinned to `19d673b357bd8c244aa1a6d17d83769224fa98c9`.
It is a relatively young project, so compatibility is defended with tokenizer IDs,
reference vectors, explicit architecture/pooling settings and cross-platform builds.

[GoMLX's Go backend](https://gomlx.github.io/docs/backends/) is another genuine Go
execution option. Its broader graph/backend stack is attractive for supporting many
architectures, but adds integration work beyond this one fixed BERT encoder.
Wrappers that call a downloaded native ONNX shared library through Go do not meet
the requirement for pure Go inference.

The application embeds every required model asset in the executable. On first use
it extracts and verifies those bytes under the XDG cache directory because the
inference library accepts filesystem directories. It passes an absolute existing
path to the loader. No Hugging Face model identifier, token, remote provider or
network request is used at runtime.

## Measurements on this machine

These are synthetic, warm CPU measurements, not production latency guarantees.
One engine thread encoded repeated `software` tokens. Ten timed calls per length
were run after warmup; values are the median in milliseconds. The model comparison
used the same pinned inference engine and FP32 execution for all four models.

| Model | 16 tokens | 64 tokens | 200 tokens |
| --- | ---: | ---: | ---: |
| MiniLM L6 | 9.07 | 21.89 | 82.87 |
| Arctic XS | 6.23 | 20.08 | 54.41 |
| GTE Small | 9.05 | 27.97 | 110.65 |
| BGE Base EN v1.5 | 34.18 | 98.16 | 300.09 |

All four models returned the intended result for ten simple English paraphrase
queries over ten synthetic ticket descriptions. Arctic and BGE queries included
their published instruction. This easy fixture establishes that the pipelines work;
it does not distinguish their quality on real tickets. Standard MiniLM passed all
11 reference-vector and tokenizer cases at cosine similarity at least 0.99999.
GTE matched its separate reference set at minimum cosine 0.9999999999994;
BGE Base matched at minimum cosine 0.9999999999763. This numerical agreement
checks inference correctness, not the quality of the model's relevance judgments.

Pinned comparison revisions: MiniLM `1110a243fdf4706b3f48f1d95db1a4f5529b4d41`,
Arctic `d8c86521100d3556476a063fc2342036d45c106f`,
GTE `17e1f347d17fe144873b1201da91788898c639cd`, and
BGE Base `a5beb1e3e68b9ab74eb54cfd186867f64f240e1a`.
Alternative model assets were used for research only and are not bundled.

Granite R2 revision `2ab6fa8ea2d674564defd37171ae19079b864b33` was downloaded,
but loading failed with `position_embedding_type="relative_key_query" — only
absolute is supported`. The model uses ModernBERT rotary/local/global attention;
changing this config field to bypass the guard would not validate that architecture.
Its tokenizer, attention and pooling must pass reference-vector checks before use.

A separate end-to-end SQLite index benchmark used 100 synthetic tickets, with one
title and one 64-token body passage each, and durable per-passage checkpoints:

| Workers | Elapsed for 200 passages |
| --- | ---: |
| 1 | 2.68 s |
| 4 | 0.665 s |
| 8 | 0.385 s |
| 16 | 0.310 s |

The first worker setting includes initial inference scratch allocation. Repeated
runs, thermal limits, background tasks and different text lengths can change these
numbers. Run the included benchmarks on the deployment machine:

```sh
go test ./internal/embedding -run '^$' -bench BenchmarkMiniLM -benchtime=10x -benchmem
go test ./internal/store -run '^$' -bench BenchmarkSemanticIndex -benchtime=1x -benchmem
```

## Retrieval design and limits

Each normalized 384-dimensional FP32 vector uses 1,536 bytes before SQLite overhead.
Twenty thousand passages use about 30.7 MB for coordinates; 100,000 use about
153.6 MB. Excerpts, row indexes and raw ticket data add storage. Chunk count depends
on bodies and comments, not just issue count.
At 768 dimensions, coordinates double to 3,072 bytes per passage. Model weights,
inference working memory and SQLite vector storage are separate costs; none is
limited to 100 MB.

The current implementation performs an exact cosine scan over locally filtered
SQLite vectors and uses the best passage score per ticket. Hybrid search fuses two
independently retrieved rankings with reciprocal rank fusion, constant 60. It does
not rely on shared BM25/cosine score scales or limit semantic search to lexical hits.
Counts and facets describe the selected ticket universe; scores are ranking values,
not duplicate probabilities. Very large corpora may justify an ANN index after
measurement, but an exact scan preserves snapshot portability and avoids an extra
native extension or service.

Labels and technical IDs are particularly suited to literal matching. Embeddings
can confuse version numbers, negation and superficially similar symptoms. English
model behavior on other languages or source code should be evaluated separately.
Use judged ticket queries with synonyms, rare client labels, acronyms, inline
comments, long-document tails, closed duplicates and unrelated hard negatives.
Compare lexical, semantic and hybrid P@10, Recall@20 and latency with `evaluate`.
Prefer an actual held-out ticket evaluation over the synthetic fixture or a general
leaderboard before replacing the bundled model.
