# Benchmark and retrieval evaluation, 2026-10-10

Hybrid search recovered more known duplicates than either individual engine in this
evaluation. Indexing scales well to sixteen workers on the tested laptop. Query
latency needs improvement: full-snapshot semantic searches currently take several
seconds, with most CPU time spent in SQLite rather than embedding inference.

Measurements used the retrieval implementation at commit `d98a992` (1.14.4), Go
1.26.4, bundled FP32 MiniLM L6, and an AMD Ryzen AI 9 HX PRO 370 with twelve physical
cores, twenty-four logical CPUs and approximately 64 GiB RAM. Benchmarks ran
sequentially; brief CLI build work overlapped a few evaluation requests. These are
local measurements with a warm filesystem cache, not a concurrency/load test or a
measurement of GitHub collection time. No production database was modified, and
the benchmark made no GitHub or model-service requests.

The immutable snapshot contained 17,985 tickets: 8,430 issues and 9,555 pull
requests. It held 57,066 comments, 83,544 indexed documents and 119,582 vector
passages, with no pending documents. Its standalone file was 700.3 MiB. Vector
coordinates alone occupy 175.2 MiB before row indexes, excerpts and other data.

The accuracy proxy came from thirty closed issues with comments explicitly stating
“duplicate of” and linking an existing ticket in the same repository. Extraction
required a statement at the beginning of a line, excluded self-links, titles with
ticket references, and one source body already referencing its target. Each case
had one known target; twenty-nine targets were distinct. Seed comments were disabled
for candidate retrieval so the annotation itself was not used as seed text. Results
were scoped to the source repository, included closed history, and excluded the
source ticket. Engine order rotated between cases. Rankings and other settings were
not tuned using these cases.

These references have not been independently reviewed. The annotation and other
historical discussion remain searchable in target documents, so this is a
retrospective recovery test rather than a temporally held-out evaluation. It covers
issue duplicates, not pull-request duplicate quality. Unmarked results are unjudged,
not confirmed negatives. The set cannot establish precision, false-positive rates,
an automatic duplicate threshold, or accuracy on arbitrary user queries.

Default candidate retrieval includes seed title, body and labels, with no seed
comments. Each engine returned its top twenty tickets:

| Engine | Known target first | Recall@5 | Recall@10 | Recall@20 | MRR@20 | Median latency | p95 latency |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Lexical | 6/30 (20.0%) | 43.3% | 53.3% | 18/30 (60.0%) | 0.298 | 924 ms | 1,335 ms |
| Semantic | 7/30 (23.3%) | 40.0% | 53.3% | 18/30 (60.0%) | 0.334 | 1,496 ms | 1,782 ms |
| Hybrid | 10/30 (33.3%) | 60.0% | 70.0% | 23/30 (76.7%) | 0.434 | 1,946 ms | 2,317 ms |

Here Recall@K measures recovery of the one annotated target, and MRR@20 averages
the reciprocal of its rank, using zero when absent. Precision@10 would be capped
at 10% by the sparse judgments; reporting that as overall precision would be
misleading. An illustrative Wilson interval for hybrid Recall@20 is 59.1–88.2%,
before accounting for annotation selection bias and repeated targets. Thirty cases
provide an initial comparison, not a precise production-quality estimate.

Lexical and semantic each recovered eighteen targets, but only thirteen were shared.
Their union recovered twenty-three. Hybrid recovered twenty-three too: it lost one
target available in an individual engine's top twenty and recovered one outside
both top-twenty lists. This supports retaining both retrieval signals. It does not
establish MiniLM as the best model for this domain.

Using only the source title as a query, while still searching all indexed fields:

| Engine | Known target first | Recall@20 | Median latency | p95 latency |
| --- | ---: | ---: | ---: | ---: |
| Lexical | 6/30 | 19/30 (63.3%) | 566 ms | 801 ms |
| Semantic | 9/30 | 17/30 (56.7%) | 1,504 ms | 1,883 ms |
| Hybrid | 8/30 | 22/30 (73.3%) | 1,819 ms | 2,127 ms |

Two additional comparisons used the same cases:

| Setting | Lexical Recall@20 | Semantic Recall@20 | Hybrid Recall@20 |
| --- | ---: | ---: | ---: |
| Default candidates with seed labels | 18/30 | 18/30 | 23/30 |
| Candidates without seed labels | 19/30 | 17/30 | 22/30 |
| Title query, all fields | 19/30 | 17/30 | 22/30 |
| Title query, search title/body fields only | 13/30 | 16/30 | 21/30 |

Removing seed labels changed results in both directions. Hybrid's known target
ranked first in ten cases with labels and seven without them. Labels remain useful,
but this set is too small to justify changing their weight. Restricting title-query
searches to title/body reduced hybrid median latency from 1,819 to 1,266 ms while
losing one recovered target. Comments and labels should remain available; field
selection is a tradeoff rather than a universal improvement.

A separate natural-language probe across both repositories took 4.77 seconds for
semantic search and 4.12 seconds for hybrid. Restricting the same query to one
repository reduced these to 1.71 and 1.79 seconds. These are individual warm-process
observations, not latency percentiles. A known technical-term lexical search returned
twenty results in a median 233 ms over three complete CLI invocations. A zero-literal-
match identifier probe returned no lexical results but still twenty semantic/hybrid
results; similarity retrieval currently has no abstention threshold. Those semantic
results were not judged, and their existence is not evidence of relevance.

Profiling three full-snapshot semantic searches collected 14.01 CPU seconds. A
disjoint classification of sampled stacks assigned 86.7% to SQLite execution or
driver work, 3.8% to vector arithmetic, 0.1% to encoding, and 9.4% to other Go work.
SQLite work includes runtime and memory functions below database frames. One
especially expensive leaf was JSON text-to-blob translation, at 11.8% of samples.
The vector scan extracts issue creation timestamps from raw JSON for every passage,
including queries sorted by relevance. Repeated payload parsing, joins and row
decoding are concrete targets for improvement. Peak RSS during the thirty-case
evaluation was approximately 244 MiB; this excludes the operating system's filesystem
cache and is not a concurrent-service memory bound.

Indexing measurements used FULL synchronous checkpoints and the current write queue.
A deterministic sample of 256 stored passages, selected with seed `20261010`, was
inserted into temporary fixtures as separate body documents. Rechunking produced
258 passages. The sample's input token lengths had median 35, mean 87.7, p95 256,
and maximum 256. Results below are medians of three complete index runs per worker
setting, excluding fixture creation and initial model loading:

| CPU workers | Index time for 258 passages | Passages/second | Unchanged rerun |
| ---: | ---: | ---: | ---: |
| 1 | 20.642 s | 12.5 | 1.56 ms |
| 4 | 4.386 s | 58.8 | 1.38 ms |
| 8 | 2.515 s | 102.6 | 1.57 ms |
| 16 | 1.685 s | 153.1 | 1.33 ms |

Sixteen workers were about 49% faster than eight in this sample. All twelve runs
completed without lock errors. Unchanged reruns performed no inference. This fixture
preserves actual text lengths but not the original multi-passage document structure,
raw payload sizes or collection workload. Extrapolating its throughput to 119,582
passages gives approximately nineteen minutes at eight workers or thirteen at
sixteen; these are estimates, not measured full-rebuild durations. Incremental
updates process only changed passages.

The existing short-text synthetic benchmark used one hundred tickets and two hundred
passages. Six repetitions per setting measured approximately 36, 164, 274 and 413
passages/s at one, four, eight and sixteen workers. Its short texts explain why it
is faster than the sampled real-text workload. Single-thread encoder measurements
over twenty inferences per length had medians of 14.2 ms for sixteen repeated
words, 48.6 ms for sixty-four, and 187.8 ms for two hundred, plus framing tokens.
Model loading plus first
inference took about 326 ms in the initial search process.

The next improvements should address the measured query cost: avoid parsing ticket
JSON per vector, fetch excerpts and ticket metadata after selecting results, and
evaluate a compact exact-scan vector cache keyed by snapshot/index generation.
Measure those changes before adding ANN dependencies. Ranking work should compare
field-aware scoring and reranking against reviewed positives and hard negatives.
A larger model remains an experiment; these results show that semantic-only
retrieval is not currently better than lexical retrieval for this proxy.

MiniLM is intended for sentences and short paragraphs, and the upstream default
truncates after 256 word pieces. gh-mirror's chunking preserves longer text, but
does not by itself establish ticket-duplicate quality. See the
[upstream model card](https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2)
and [model research](semantic-model-research.md). The
[BEIR paper](https://arxiv.org/abs/2104.08663) also motivates evaluating retrieval
across domains rather than treating a generic benchmark as local ground truth.

For a stronger accuracy assessment, build a separately reviewed set covering
natural-language queries, exact identifiers, client labels, negation, version
changes, unrelated similar symptoms, inline review comments, long-document tails
and cross-repository cases. Judge pooled results from every engine, include
explicit negatives, and evaluate on a held-out subset. Preserve the snapshot and
judgments so comparisons use the same data. Existing CLI `evaluate` can measure
P@10 and Recall@20 against those judgments.

Reproduce the public synthetic benchmark with:

```sh
go test ./internal/store -run '^$' -bench BenchmarkSemanticIndex -benchtime=3x -count=2 -benchmem
go test ./internal/embedding -run '^$' -bench BenchmarkMiniLM -benchtime=20x -benchmem
```

Private snapshot paths, source text, ticket identities, annotations and raw ranked
results are retained only in ignored local benchmark artifacts. This report contains
aggregate measurements. Reproducing the historical recovery test requires access
to the same private snapshot and judgments.
