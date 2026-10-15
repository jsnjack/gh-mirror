# gh-mirror

Read [universal conventions](AGENTS.universal.md) and [Go conventions](AGENTS.go.md).
The specification is in [docs/spec.md](docs/spec.md). Run `make build` after runtime
changes and `make check` before reporting completion. Do not install missing tools,
commit, push, or change another repository without user authorization.

`cmd/` parses Cobra commands and wires dependencies. Help groups commands as Query
(search, list, show, comments, context, related, catalog), Collect (sync, index,
snapshot, acquire), Serve (serve, mcp), Inspect (status, doctor, scope, bots, model,
config) and Advanced (evaluate). Ticket commands share REF parsing (`owner/repo#N`,
GitHub URLs, `#N` with `default_repo` or one configured repository) and keep hidden
`--repo/--number`. Hidden deprecated `get`, `batch`, `candidates` and `--label`
keep scripts working. CLI query views default to summary. `status` reports `db_path`
and `pending_sync`; `doctor` exits 1 on failed checks. Completion reads configured
repositories and local labels without failing when either is missing. `internal/config` loads TOML configuration
and resolves XDG paths; JSON is rejected. `internal/diagnostics` manages stderr and trace
logging; the trace file is per user and refuses links. `internal/progress` renders sync activity from explicit collector/client
callbacks. `internal/github` performs bounded read-only upstream requests and pagination.
`internal/collect` synchronizes configured resources. `internal/store` owns the SQLite
schema, transactions, FTS5 index, and queries. `internal/snapshot` publishes and acquires
immutable databases. `internal/service` shares local queries between REST and MCP.
`internal/checkpoint` durably stages completed responses outside the mirror transaction
so interrupted syncs resume across process boundaries.
TOML `repos` tables resolve eleven boolean resource switches per repository using
all/issues/pull_requests presets with boolean overrides, or global defaults when the
preset is absent. Legacy `repositories`/`repository_options` keys are rejected.
config show emits compact/effective TOML. Keep examples/config.toml listing every
setting; `TestExampleConfiguration` enforces it. The pure Go go-toml parser is justified by the requested TOML configuration.
`scope` prints effective options and committed status records them.
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
Only successful collection transactions advance checkpoints. List issues and comments
by creation time so edits do not shift numbered pages. Before reconciliation removes
an in-scope ticket a complete listing omitted, `Client.Lookup` must confirm it absent
(404/410/301); returned tickets are kept. A null hydration node must be confirmed
the same way, then clears that repository's reconciled_at instead of failing the sync.
The enrichment label refresh refetches tickets carrying renamed or deleted labels,
found through the committed mirror. Recovered comment parents outside scope record
only their kind. Record project coverage
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
progress callbacks. Retry 429/secondary 403 and GraphQL RATE_LIMITED with at least a
one-minute backoff for five attempts, and wait for quota reset up to 61 minutes.
Client clock and sleep are injectable for tests. GraphQL errors keep type and message. Parallelize independent listings, advertised numbered REST page
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
watermark on resume, but evaluate enrichment and reconciliation intervals against
the current time. Failed attempts record a count and bounded error in the checkpoint;
`checkpoint.Inspect` reads them without creating or writing the file. Fingerprint scope/settings/credential identity without storing
credentials; worker/budget changes preserve pending work. Initialize and clean up
sessions under the collector lock. Never discard staged work before the mirror commits
or delete another collector's session. Snapshot exports exclude pending work.
Validate GraphQL node identities, resource kinds, connections and pagination cursors
before saving responses. Refetch invalid legacy saves by deleting only that request;
retain valid saved pages and batches when a later request fails.

Readers open existing databases without migrations or writes. Publish standalone
SQLite exports, never copies of live WAL databases. Complete and validate the export
before atomically replacing `latest.json`, then prune all but the three newest
snapshot files. Same-host consumers copy the named file; `acquire` serves remote hosts. Acquisition resolves the manifest once,
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

`sync --if-stale` skips when `Status.CollectionAge` is below `stale_after`, or when
`collect.CheckFreshness` finds the mirror write lock held with a checkpoint session;
`collect.Options.StaleAfter` repeats the age check under the lock and busy errors map to running.
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

CI runs format, vet, build, race tests and lint on pushes and pull requests.
`make release` runs locally: it requires a clean, pushed HEAD, packages CGO-free
Linux/macOS amd64/arm64 archives with SHA-256 checksums, and uploads them with
`grm release` under tag `v<monova>`. Archives include the executable, README and
bundled model notices/license; never package collector data or user configuration.

Dependencies are justified by the accepted design: Cobra is required by standards;
modernc.org/sqlite provides embedded SQLite/FTS5 without CGO; the official MCP SDK
provides protocol and transport handling, and its jsonschema-go package builds the
described tool input schemas. The existing go-isatty dependency detects
terminal writers without adding a UI framework. Tests use local HTTP fixtures and temporary
databases. No live GitHub writes or production WakeCI changes belong in validation.

Local queries share composable predicates and generation-bound cursors. Lexical
search keeps literal OR matching by default; all-word mode requires terms across
the whole selected ticket, and phrase mode requires consecutive terms in one document.
Compile only generated, escaped FTS expressions. Report term limits explicitly.
Search/list support deterministic sorting, summary/full projections and optional
counts/facets before pagination. Preserve legacy raw fields; every engine reports
higher-is-better scores and relevance defaults to descending. Lexical scores are
negated BM25 and lexical cursors bind `lexicalScoreVersion`. Attach bounded
evidence and a ranking description. Date bounds accept RFC3339, YYYY-MM-DD and
relative h/d/w ages resolved at query time. `store.CatalogKinds` lists valid kinds. Lexical query enhancements must
work against existing snapshots without migrations or upstream requests; semantic
engines explicitly require a compatible local vector index when requested by name.
An omitted search/candidate engine selects hybrid, falling back only for
ErrSemanticUnavailable with a structured search_engine_fallback warning and the
actual engine/ranking. Default all/phrase/prefix searches select lexical with an
explanation. Preserve lexical candidate term selection on fallback and return
typed stale-cursor conflicts when the effective engine changes between pages.
CLI flags advertise hybrid but preserve whether the user explicitly chose an
engine; REST/MCP use the same store policy. Keep all sources and open/closed history
available by default. Agent retrieval supports ticket evaluation, behavior,
decisions, workarounds and fixes; summaries/evidence are entry points for bounded
detail/comment reads, and retrieval scores do not establish relevance or duplicates.
Comment pages separate discussion and review kinds, preserve large numeric IDs as
strings and namespace cursors independently from ticket pages. Compact comment
previews limit Unicode characters and report truncation; full view retains raw
payloads. Batch reads use one transaction, omit comments and report missing IDs.
Legacy full ticket/catalog reads remain; clients opt into bounded pages. MCP tools
keep stable names and parameters, describe every input field with enums for fixed
choices, advertise no output schemas, and replace result `status` with generation
and collected_at (get_sync_status keeps it whole). `mcp` lists seven core tools;
`--tools all` adds listing, catalog and project tools. MCP `get_issue` defaults to
summary without comments. REST uses
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
loopback process with `--offline`, a private `--api-key-file` and Linux Pdeathsig; do not contact a shared model daemon, download
models, or inherit LLAMA/GGML inference overrides. Verify full layer offload, exact
bundled token IDs, reference vectors and a full 256-token passage before accepting
GPU results. Compatibility requires cosine >=0.9999 and coordinate error <=0.001;
validate every batch's model, indices, dimensions and normalized finite values.
Failures retry the same model on CPU and report the reason. Limit CPU fallback
concurrency across calls; bound GPU batches and queues and join them on shutdown.
Pass the encoder explicitly through `OpenWithVectorizer` for REST/MCP/CLI queries.
CPU/Vulkan switching preserves fingerprints, collection sessions and saved vectors.
Config `embedding` and `--embedding-*` flags on text-encoding commands select backend, runtime, device and batch
bound; `model --check` verifies the selected runtime offline without opening a mirror.

Schema 2 adds semantic state/vectors; schema 3 stores shared vectors separately
from source passages. Writer migrations from schemas 1/2 are local and transactional.
Readers accept schemas 1–3 without migrations. Keep CollectionVersion unchanged for this derived index. Index only
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
cosine scans and lexical/semantic/whole-ticket rank fusion (k=60, weights 1/1/3); higher scores rank
first. Report source offsets/similarity and typed 503 errors for missing/incomplete
indexes on explicit semantic/hybrid requests. Bind vector cursors to vector generation
and fingerprint as well as collection and query. Semantic candidates reuse weighted
cached seed vectors.
Bind semantic cursor generations to the actual query-vector hash so backend
numerical changes reject continuation with the typed stale-cursor error.
Semantic retrieval keeps `semanticCandidateLimit` tickets; hybrid adds every lexical
and ticket-evidence match, so totals and facets follow that set. Research model alternatives against primary
sources and target-hardware measurements; synthetic retrieval fixtures do not prove
production duplicate accuracy.

`internal/representation` owns local label roles, section byte offsets and typed
explicit references while keeping upstream text unchanged. Repository `label_rules`
are saved during local indexing, independent of collection compatibility and model
identity. Summary metadata promotes state_reason, closed_at, draft and nested PR
merged_at. `context`, REST issue `/context` and MCP `get_issue_context` share one
read transaction with source-text budgets, selection/omission coverage and bounded
one-hop relationships. Exact description text and code remain available; missing
sections and duplicate references are observations, not model decisions.

Hybrid ranking joins literal evidence across selected ticket fields and soft
client/partner attributes, exposing matched terms, fields and component ranks.
Entity boosts require topic coverage in general search; never auto-filter other
clients. Duplicate candidates preserve weighted seed-vector and lexical fusion,
without whole-ticket entity reranking. Semantic-only scores remain maximum passage
cosines, while hybrid semantic ranks combine best passage and best-per-field
evidence. Bind cursors to the ranking version.

`cachingVectorizer` coalesces exact inputs across workers and batches, keyed by
model fingerprint, representation version and text using compact 32-byte hashes.
The shared SQLite dictionary checkpoints completed inference before source
attachments, retaining reusable values on cancellation. Source passages reference
the dictionary; migration accepts legacy inline BLOBs until the local rebuild
shares them. Version derived inputs separately from collection. Ticket body
passages exclude empty sections and known checklists while preserving raw source
offsets. Exports prune incomplete sources and unreferenced shared vectors.
Classification context projects label names and descriptions from the raw catalog,
with attached labels first and a one-third text-budget cap. Configured rules take
priority over the taxonomy. Report omitted labels in one counted warning, including
row-limit omissions. Render label interpretations, definitions, rules, custom fields
and related excerpts in text. Full label records remain available through catalog.
Bound section and relationship lists independently of body length. `internal/markdown`
owns the one fence parser for sections, references and attribution. Fences honor
delimiter type and length, retaining shorter fence examples as code; backtick info
strings cannot contain backticks. Bump `representation.Version` or the registry
version when their parsing changes.

Keep large vector BLOBs in ordinary rowid tables: WITHOUT ROWID index pages
overflow 1536-byte values at SQLite’s default page size. Migrate early dictionary
layouts locally without changing vector generations or rerunning inference.
Whole-ticket coverage reads identities and fields from FTS postings without
computing snippets or BM25. Default coverage uses a source pass and a title pass,
keeping exact SQLite token matching. The signal field ticket covers title/body
evidence; returned passages identify their exact field.

`internal/commentpolicy` owns a bundled versioned registry of known AI review
accounts and deterministic PR comment selection. Require Bot metadata for known
account aliases; numeric actor IDs are GitHub.com scoped. Also recognize explicit
final-line Claude Code attribution and the declared-AI HTML marker, outside code
and quotes, per comment regardless of account type. Keep unmarked comments and
never infer authorship from style or mere mentions. bots separates account and
attribution detection without labeling every post by a human as AI-generated.
Explicit includes take precedence.
Resolve same-ticket inline reply roots, reject cycles, and exclude an excluded root's
thread when configured; discussion comments remain individual. Preserve raw comments.
Global pr_comments and nested repository overrides resolve separately from fetch
scope and do not fingerprint GitHub resume work. Apply policies atomically during
collection and offline index by reconciling comment search documents. Cascading
vector deletions remove excluded passages; restore raw sources locally on opt-out.
Persist resolved policies/registry identity in snapshot metadata; readers never
change policy. Bounded comment/context reads follow document eligibility, with
include_excluded overrides and counted warnings. Legacy all-comment reads retain
raw history. Bind lexical/comment cursors to the local policy generation while
collection Generation remains unchanged. bots shows detected actors and actual
exclusion counts. Effective TOML explicitly renders false policy overrides and
empty author lists so copying it preserves inheritance semantics.
Evaluation pins collection, comment-policy and semantic generations/model identity,
rejecting concurrent local policy/index changes as well as collection updates.
