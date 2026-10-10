# gh-mirror

Read [universal conventions](AGENTS.universal.md) and [Go conventions](AGENTS.go.md).
The specification is in [docs/spec.md](docs/spec.md). Run `make build` after runtime
changes and `make check` before reporting completion. Do not install missing tools,
commit, push, or change another repository without user authorization.

`cmd/` parses Cobra commands and wires dependencies. `internal/config` loads JSON
configuration and resolves XDG paths. `internal/diagnostics` manages stderr and trace
logging. `internal/progress` renders sync activity from explicit collector/client
callbacks. `internal/github` performs bounded read-only upstream requests and pagination.
`internal/collect` synchronizes configured resources. `internal/store` owns the SQLite
schema, transactions, FTS5 index, and queries. `internal/snapshot` publishes and acquires
immutable databases. `internal/service` shares local queries between REST and MCP.
`internal/checkpoint` durably stages completed responses outside the mirror transaction
so interrupted syncs resume across process boundaries.
`repository_options` resolves eleven boolean resource switches per repository; omitted
flags in an explicit entry are false. Repositories without entries retain legacy
global defaults. `scope` prints effective options and committed status records them.
Filter shared issue/comment listings locally; retain only identity/kind for excluded
tickets in private `issue_inventory` to avoid fetching each comment parent. Exports
remove this inventory. PR review comments use the bulk repository endpoint, default
to disabled, and use namespaced IDs to avoid collisions with discussion comments.
Changing ticket/comment scope reconciles only that repository;
catalog/metadata changes rehydrate it without forcing full listings. Group hydration
by fields/projects/relationships and share owner catalogs only when enabled by at
least one repository. Explicit equivalent defaults preserve pending fingerprints.

Index attached label names as separate full-text documents for each issue, with
the issue URL as their source and the same weight as titles. Keep raw issue titles
and bodies unchanged. Update label documents with issue changes; removals, transfers,
and issue deletion must remove stale matches. `Writer.EnsureLabelIndex` backfills
legacy mirrors locally during sync using stored payloads and `label_index_version`.
This derived index change preserves collection compatibility and pending work and
must not force extra upstream requests. Unused labels remain catalog entries and
do not produce ticket search matches.
`Store.List` enumerates raw tickets and metadata by repository and issue number,
without comments or required search words. CLI `list`, REST `/v1/issues` and MCP
`list_issues` share generation-bound keyset cursors that reject changed filters or
generations. Search and listing share filters, including issue/pull_request kind
and project owner/number. Match stored membership URLs for organization/user
projects, enterprise hosts and archived memberships without extra collection.

GitHub remains authoritative. Never issue upstream mutations. Preserve raw payloads,
all accessible comments, native issue types, multiple assignees, and unused labels.
Collect project catalogs and ticket memberships, including archived memberships.
Do not collect complete project items, draft cards, or project field definitions/values.
Never treat failed pagination, inaccessible resources, or unsupported fields as an
empty successful collection.
Only successful collection transactions advance checkpoints. Record project coverage
as `memberships` or `disabled`; upgrading older complete project coverage preserves
existing memberships and removes obsolete catalogs without forcing enrichment.
Keep compatible request checkpoints when narrowing project collection. Poll comments
separately from issues. Bootstrap uses 100-record REST pages and 50-node GraphQL batches shared
across repositories. Hydrate only changed tickets on deltas; reconcile catalogs,
projects, fields and relationships on the configured enrichment interval. Expose
that timestamp separately. Never retain non-reusable since-response cache entries. Full reconciliation removes stale issues, comments, and memberships.
Repository comment listings may reference parents omitted from issue listings.
Recover each missing parent once before storing its comments, preserve it through
full reconciliation, and include it in batched hydration. On deltas, check the stored
inventory first. Recovery fetches use the shared budget and durable resume checkpoint;
inaccessible or mismatched parents fail explicitly without dropping comments.

`store.CollectionVersion` defines data compatibility separately from release and SQL
schema versions. Increment it when collection or normalization changes invalidate
existing data for incremental updates. Missing metadata means legacy version 1.
A mismatch forces a full inventory and enrichment rebuild, discards incompatible
conditional responses, and fingerprints pending work with the target version and
effective full mode. Keep the legacy version-1 fingerprint unchanged. Persist the
new version only when collection commits; cancellation or failure preserves the
previous mirror and resumes completed fetches for the target version. Compatible
code changes do not bump this version or force collection. Physical SQL schema
changes remain subject to the separate schema compatibility checks.
Persist an opaque hash of upstream endpoints and credentials only after successful
sync. A changed or missing identity in a committed mirror forces a resumable full
inventory and enrichment rebuild with cleared conditional caches. Do not expose
the identity through status or exported snapshots. Permission changes on the same
token still require explicit full sync or scheduled reconciliation.

Workers share concurrency permits, budget reservations, retry pauses, and serialized
progress callbacks. Parallelize independent listings, advertised numbered REST page
ranges and hydration batches; follow cursor-dependent or unknown-range pagination
sequentially. Preserve page-link query ordering for exact-URL checkpoint reuse.
Validate origins, page-series parameters and contiguous next links, and assemble
results in page order while reporting completed-page counts. Follow a growing tail
from its next link; reject inconsistent pagination rather than commit a partial
inventory. Discard only invalid page checkpoints and the range seed when its plan
needs rediscovery. Keep collection/session compatibility unchanged for scheduling.
Persist updated pagination headers from 304 responses when supplied, retaining
cached links when the response omits them.
Apply hydration results serially and join workers
before ending the collection transaction. One worker must support serial collection.
Save completed responses in the private `<database>.sync.sqlite` checkpoint with
FULL synchronous durability, including delta pages. This temporary resume store is
separate from the reusable conditional cache. Keep the original collection-start
watermark on resume. Fingerprint scope/settings/credential identity without storing
credentials; worker/budget changes preserve pending work. Initialize and clean up
sessions under the collector lock. Never discard staged work before the mirror commits
or delete another collector's session. Snapshot exports exclude pending work.
Validate GraphQL node identities, resource kinds, connections and pagination cursors
before saving responses. Refetch invalid legacy saves by deleting only that request;
retain valid saved pages and batches when a later request fails.

Readers open existing databases without migrations or writes. Publish standalone
SQLite exports, never copies of live WAL databases. Complete and validate the export
before atomically replacing `latest.json`. Acquisition resolves the manifest once,
verifies checksum, scope, schema and freshness, and installs a private local copy.
Exports remove all conditional response caches and local credential identity, then
compact the private export to remove deleted payload bytes. Prune disabled field
and project caches and obsolete full-item caches inside successful sync transactions.
Snapshot manifests include collection version; legacy manifests default to version
1. Publication and acquisition reject incompatible collection contracts and compare
the embedded version. Optional `acquire --max-enrichment-age` constrains metadata
freshness independently of comment/issue collection age.
Credentials come from named environment variables and never enter the database,
stdout, request bodies, or trace logs. HTTP listeners outside loopback require an
API bearer token. Read tools never trigger collection or expose arbitrary SQL.

Sync progress starts before opening the database and uses stderr, leaving stdout
for command results. Animate terminal output; use plain phase updates and periodic heartbeats
for nonterminals or debug mode. `sync --quiet` disables progress. Report actual
pages, records, hydration batches, HTTP attempts, conditional hits, and retry waits
without extra GitHub requests. Show bounded progress only for known local totals.
Stop refreshes on success, failure, and cancellation. Diagnostic logs must not
interleave with animated frames.
Count interleaved listing records independently and report active workers and resumed
responses separately from HTTP attempts.
Wrap terminal rows to the detected width without truncating metrics. Keep GitHub
quota and resume counters on separate rows and track physical rows when clearing
frames. Known totals show percentage, phase throughput, and a phase ETA after
measurable progress; unknown totals and retry waits do not show an ETA.

CI runs format, vet, build, race tests and lint on pushes and pull requests. Tagged
releases run those checks before packaging CGO-free Linux/macOS amd64/arm64 binaries,
verifying SHA-256 checksums and publishing assets. `make release` uses monova's
version and includes the executable, README and bundled model notices/license;
never package collector data or user configuration. Release workflows grant write
permissions only to publication.

Dependencies are justified by the accepted design: Cobra is required by standards;
modernc.org/sqlite provides embedded SQLite/FTS5 without CGO; the official MCP SDK
provides protocol and transport handling. The existing go-isatty dependency detects
terminal writers without adding a UI framework. Tests use local HTTP fixtures and temporary
databases. No live GitHub writes or production WakeCI changes belong in validation.

Local queries share composable predicates and generation-bound cursors. Search keeps
literal OR matching by default; all-word mode requires terms across the whole
selected ticket, and phrase mode requires consecutive terms in one document.
Compile only generated, escaped FTS expressions. Report term limits explicitly.
Search/list support deterministic sorting, summary/full projections and optional
counts/facets before pagination. Preserve legacy raw fields and score meanings;
attach bounded evidence and a ranking description. Lexical query enhancements must
work against existing snapshots without migrations or upstream requests; semantic
engines explicitly require a compatible local vector index.
Comment pages separate discussion and review kinds, preserve large numeric IDs as
strings and namespace cursors independently from ticket pages. Compact comment
previews limit Unicode characters and report truncation; full view retains raw
payloads. Batch reads use one transaction, omit comments and report missing IDs.
Legacy full ticket/catalog reads remain; clients opt into bounded pages. MCP schemas
describe stable output envelopes with unconstrained raw upstream JSON. REST uses
400 for invalid input, 404 for absent resources, 409 for stale generation cursors
and 500 for internal failures. Query warnings explain disabled, stale and uncollected
resources without making network requests.
Candidate retrieval preserves short technical terms, de-duplicates template words
and ranks seed terms using title/label/comment weights and local ticket frequency.
Default scope is the seed repository; explicit repositories widen it. Seed labels
are enabled unless explicitly false; recent seed comments are opt-in and bounded.
Candidate reads never load all seed comments or query GitHub. Offline evaluation
uses explicit local relevance judgments, reports P@10 with a fixed denominator of
10 and Recall@20, and fails on generation changes. Synthetic fixtures test retrieval
semantics; their scores do not establish quality on real tickets. Benchmark broad,
selective and candidate queries before changing query plans or adding dependencies.

`internal/embedding` embeds the pinned Apache-2.0 MiniLM L6 weights/tokenizer and
verifies assets before loading their absolute local cache path with pinned rembed.
The dependency is justified by explicitly requested pure Go offline inference;
never pass remote model IDs or silently download assets at runtime. FP32, mean
pooling, L2 normalization, tokenizer and 256-token/2048-rune/32-rune-overlap chunking
form one fingerprint. Validate tokenizer IDs and vectors against attributed reference data.
`embedding.GGUF` exports those exact FP32 tensors and vocabulary to llama.cpp's
BERT format locally, with deterministic metadata and checksum validation. It adds
no conversion dependencies and does not alter the model fingerprint.
`embedding.Provider` defaults to the bundled CPU encoder. Opt-in `lemonade-vulkan`
starts Lemonade's installed llama.cpp Vulkan executable as a dedicated authenticated
loopback process with `--offline`; do not contact a shared model daemon, download
models, or inherit LLAMA/GGML inference overrides. Verify full layer offload, exact
bundled token IDs, reference vectors and a full 256-token passage before accepting
GPU results. Compatibility requires cosine >=0.9999 and coordinate error <=0.001;
validate every batch's model, indices, dimensions and normalized finite values.
Failures retry the same model on CPU and report the reason. Limit CPU fallback
concurrency across calls; bound GPU batches and queues and join them on shutdown.
Pass the encoder explicitly through `OpenWithVectorizer` for REST/MCP/CLI queries.
CPU/Vulkan switching preserves fingerprints, collection sessions and saved vectors.
Config `embedding` and persistent flags select backend, runtime, device and batch
bound; `model --check` verifies the selected runtime offline without opening a mirror.

Schema 2 adds semantic state/vectors; writer migration from schema 1 is local and
transactional. Readers accept schema 1 for lexical queries and schema 2 without
migrations. Keep CollectionVersion unchanged for this derived index. Index only
pending documents, reuse identical passages after edits, and checkpoint vectors
with FULL durability. Guard writes against changed text/model; cascade deletions.
Incomplete states are excluded from semantic queries and stripped from exports,
which retain completed vectors and record model identity in manifests. Local
resumable passages remain in the collector database. Query-only paths never index.

`index` works offline. `sync` indexes after collection commits and before publishing,
unless --index=false; CPU workers are bounded separately from GitHub workers.
Indexing inherits the effective sync/configured worker count unless explicitly
overridden with sync --index-workers or standalone index --workers. Keep document
workers alive across keyset pages with a bounded queue; close read rows before
dispatch, join producers/workers on cancellation, and rescan after a completed pass.
Queue same-store write transactions before acquiring a connection, with cancellable
waiting; SQLite has one WAL writer. Keep inference and readers concurrent, retain
FULL checkpoint durability and use SQLite locking to exclude independent collectors.
Optional `BatchVectorizer` bounds passage batches within documents and queries.
Drain completed batch results into checkpoints even after cancellation; retain the
first error while saving other successful results. Report the actual runtime,
device, computed passage throughput, batches and fallback reason through explicit
provider callbacks; progress keeps document workers distinct from inference batches.
Serialize CPU activity snapshots and progress callbacks; label CPU/GitHub phases
explicitly. Preserve vector fingerprints and reusable checkpoints for scheduling changes.
Progress names standalone indexing correctly and retains stderr/stdout separation.
CLI --format auto selects text only for terminal stdout; piped output stays JSON.
Explicit json/text overrides detection. Render search/candidate evidence, labels,
pagination, warnings and evaluation tables in text; preserve integer identities,
sanitize terminal controls and propagate output errors. REST/MCP are unaffected.
Semantic/hybrid queries share ticket filters and result contracts. Use exact filtered
cosine scans and independent lexical/semantic rank fusion (k=60); higher scores rank
first. Report source offsets/similarity and typed 503 errors for missing/incomplete
indexes. Bind vector cursors to vector generation and fingerprint as well as collection
and query. Semantic candidates reuse weighted cached seed vectors.
Bind semantic cursor generations to the actual query-vector hash so backend
numerical changes reject continuation with the typed stale-cursor error.
Keep lexical behavior and score direction unchanged. Research model alternatives against primary
sources and target-hardware measurements; synthetic retrieval fixtures do not prove
production duplicate accuracy.
