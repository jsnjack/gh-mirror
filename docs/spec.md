# gh-mirror specification

## Purpose

gh-mirror collects GitHub ticket data into a local SQLite database for personal
tools and small organizations. GitHub remains the source of truth. Duplicate
candidate discovery and queries use local indexes; upstream changes belong to the
consumer and its review/recovery workflow.

## Deployment

The application is a Go executable built without CGO. It requires no external
database, search engine, queue, or model service. MiniLM weights and tokenizer are
bundled for pure Go CPU inference. Cobra follows workspace
standards. modernc.org/sqlite supplies SQLite and FTS5 without platform-specific
C libraries. The official Go MCP SDK handles stdio and Streamable HTTP.

The CLI supports `sync`, `index`, `model`, `search`, `get`, `candidates`, `catalog`, `status`, `snapshot`,
`acquire`, `list`, `scope`, `serve`, and `mcp`. All commands inherit `--config`/`-c`, `--debug`/`-d`,
and `--trace`. The root supports `--version`. Configuration is JSON under the XDG
configuration directory. Durable data uses the XDG data directory. Tokens are
read from named environment variables, never configuration values or databases.

## Data contract

Configured repositories include open and closed issues and ordinary PR conversation
records, distinguished by kind. Preserve upstream JSON, complete Markdown bodies,
authors, timestamps, URLs, state reasons, assignees, labels, milestones, and native
issue types. PR approval reviews, full PR details, commits, file diffs, check runs and attachment
binaries are outside this version. Inline PR review comments are optional and
preserve diff context and reply identifiers; discussion comments are independent. Preserve attachment links.

Collect all accessible repository comments with stable IDs. Catalogs include unused
repository labels and milestones, organization issue types, and organization issue
field definitions/options. Native issue values are collected in batched GraphQL
node queries with pagination. Project catalogs include accessible owner projects.
Ticket memberships preserve project ID, number, title, URL, and archived state
through the ticket's paginated GraphQL connections.
Complete project item inventories, draft cards, and project field definitions/values
are outside the collection scope.

Native issue fields remain distinct from project membership metadata. Extra issue
GraphQL metadata includes parent, sub-issues, blocked-by, blocking, and project
memberships. Every nested connection must be paginated. Unavailable organization features can be
explicitly disabled in configuration and are recorded as disabled, never complete.
Personal repositories do not have organization issue catalogs.

The mirror captures current accessible state rather than historical deleted text
or every revision. Upstream collection is not a globally simultaneous snapshot.
Publishing produces a transactionally consistent local snapshot of observations.

## Collection

Bootstrap uses paginated repository listings, never GitHub Search. Issues and
repository comments have independent updated-time checkpoints with a five-minute
overlap. A successful checkpoint records collection start time; changes during
collection remain eligible for the next run. Upserts reject stale timestamped
responses, replace label membership, and update the index in the same transaction.

Comment listings can reference issues absent from the bulk issue inventory. Recover
each missing parent through a single issue GET before storing its comments. Deltas
use the existing stored inventory to avoid fetching unchanged parents. Recovered
issues remain in full reconciliation inventories and receive batched metadata
hydration. Recovery shares the request budget and response checkpoint, so retrying
after interruption reuses the fetched parent. Inaccessible or mismatched parent
responses fail collection rather than dropping comments.

New and changed tickets receive batched field/relationship hydration. Unchanged
overlapping REST payloads do not trigger hydration. Native field values, relationships,
project membership, and owner catalogs are also reconciled independently of issue
update timestamps every `enrichment_interval` (default one hour). The last complete
enrichment timestamp is exposed separately from issue/comment collection time.
Project memberships use the ticket metadata query with `includeArchived:true` and
complete nested pagination. Owner project catalogs are fetched once per owner.
Project coverage is recorded as `memberships` or `disabled`. Existing mirrors whose
project coverage was `complete` retain existing membership metadata and remove
obsolete project detail catalogs without forcing enrichment. Narrowing collection
reuses compatible request checkpoints.
Periodic full issue/comment reconciliation detects removed records. `--full` requests
it explicitly; the default reconciliation interval is 24 hours. A full listing removes
unseen records only after every page succeeds. A 404 or 403 is an access/collection
failure rather than proof of deletion. Removing a configured repository removes its
records at the next successful sync and changes published scope.

One SQLite immediate write transaction serializes collectors across processes and
commits configured repositories, catalogs, indexes, and checkpoints together.
Bootstrap lists issues and repository comments in pages of 100 and hydrates at most
50 tickets per GraphQL batch, shared across repositories. Owner catalogs/projects are collected once per owner.
Between enrichment and full inventory refreshes, an unchanged repository requires
two HTTP requests. A 251-ticket fixture with complete catalogs, one project, and a
nested overflow requires 17 bootstrap requests. Exact conditional responses retain
ETags and Link headers for reusable listing/catalog URLs. Delta URLs with changing
`since` watermarks are not persisted in the HTTP cache; conditional requests still count against the request budget.
Use advertised numeric next/last page ranges to dispatch bounded page workers through
the shared request pool. Preserve query ordering in generated URLs for checkpoint
compatibility, checkpoint each finished page, report monotonic completed-page counts,
and assemble results in page order. Validate origins, series parameters and next-page
continuity; fail incomplete inventories. Follow extensions beyond the advertised tail
using their returned links. Unknown ranges and cursor pagination remain sequential;
do not speculate beyond known ranges. Join page workers before returning on any error
or cancellation, preserving successfully saved pages. Scheduling changes leave the
collection version and pending-session fingerprints unchanged.
Network errors, invalid payloads, GraphQL errors/partial responses, repeated pagination
cursors, or request-budget exhaustion roll back the collection. Readers use WAL
transactions. HTTP retries have bounded attempts and wait time, obey Retry-After and
rate-limit reset headers, and respect cancellation. Requests have deadlines and a
shared per-sync budget. Trace logs record resource/count/status metadata without
credentials or ticket bodies. No upstream mutation API exists.

Fetch concurrency defaults to four workers and is bounded to 1–16 through `workers`
or `sync --workers`. Independent issue/comment listings and 50-node hydration batches
run concurrently; dependent pagination remains sequential. One client coordinates
REST, GraphQL, retries, budget reservations, and rate-limit pauses. Worker failures
cancel and join the pool before the collection transaction can end. Hydration
results are applied serially. One worker selects serial collection.

Completed REST pages (including resolved conditional bodies, ETags, and next links)
and successful GraphQL data are committed to a separate private SQLite checkpoint
at `<database>.sync.sqlite` with FULL synchronous durability. Interrupted collection
rolls back the visible mirror while retaining these fetches, even across process
termination. Resume replays saved responses and requests only missing work; reused
responses do not consume the current invocation's request budget. Pending work keeps
its original collection start timestamp so the next delta can cover changes made
while collection was stopped. Only a completed sync advances visible coverage.

An opaque session fingerprint covers the previous generation, scope, upstreams,
collection settings, full mode, credential identity, and checkpoint format version.
Credentials themselves are never persisted. Changed worker count or request budget
does not invalidate a session; incompatible settings replace pending work.
`sync --restart` discards the unfinished session explicitly. Session setup and cleanup
hold the live database's collector lock; cleanup verifies session ownership after
the mirror commit. Checkpoints are excluded from snapshots and removed after success.

The persisted `collection_version` identifies data compatibility independently of
release and physical SQL schema versions. Missing metadata denotes legacy version 1.
Increment `store.CollectionVersion` when collection or normalization changes make
existing data unsafe for deltas. A mismatch automatically forces complete issue and
comment inventories plus metadata/catalog enrichment, resets incompatible stored
records and conditional responses within the transaction, and reports the reason.
The effective full mode and target collection version identify the pending session.
Version-1 fingerprints retain their original encoding to preserve legacy work.
Only a successful commit records the target version; interruption retains the old
published generation and resumable fetches for the rebuild. After success, the next
sync returns to ordinary incremental operation. Compatible code changes do not
change this contract version. Generic API/network failures do not cause a rebuild.
Physical SQL schema compatibility is checked separately before collection.

Sync reports progress on stderr by default, starting before database setup. A
terminal receives a refreshed display of phase, repository, listing counts,
elapsed time, request usage, conditional cache hits, and rate-limit information.
It also reports active/configured workers and saved responses reused on resume.
Terminal rows wrap to the current terminal width without truncating metrics; quota
and resume counts occupy separate rows. Changing frame heights erase obsolete rows.
Known indexing/hydration totals have progress bars, percentage, phase throughput,
and a phase ETA after measurable progress. Unknown totals and retry waits have no
ETA; listings with unknown totals have a spinner. Explicit callbacks carry activity from the collector and client
without additional upstream calls. Status refreshes during slow requests, retries,
and snapshot publication. Nonterminal/debug output uses plain phase updates and
periodic heartbeats. `sync --quiet` disables progress. CLI --format auto chooses
readable text for terminal stdout and JSON for pipes/files, with explicit json/text
overrides. Text queries show ranked tickets, labels, bounded evidence, warnings and
continuation; evaluations show a metrics table. Preserve complete JSON contracts,
integer identities and output errors. REST/MCP always remain structured.
Success, failure, and cancellation stop refreshes and produce a final status.

## Retrieval

FTS5 indexes issue titles/bodies and individual comments. Results return issue
identity, source URL, matched snippet, ranking, timestamps, and collection status.
Literal user words form an OR query for candidate recall; raw FTS/SQL is not exposed.
Filters include repository, state, label, and native issue type. Candidate retrieval
derives distinctive terms from a stored ticket, excludes that ticket, includes closed
history, and returns ranked suggestions. Scores are retrieval scores, not duplicate
probabilities. No automatic duplicate adjudication or closure belongs in gh-mirror.

Full-text search includes issue titles, bodies, attached label names, and comments.
Label documents preserve the issue URL as evidence and have the same BM25 weight
as titles. Label changes, transfers, and deletion remove stale matches. Existing
mirrors receive a transactional local backfill from stored payloads on the next
sync, recorded by `label_index_version`, without invalidating pending work or
forcing extra upstream requests. Unused labels remain catalog data.

`get` returns the complete stored issue, comments, native field values, relationships,
and project memberships. Project queries return the local project catalog record
and collection status. REST and MCP call the same query functions. Reads report the
database generation and resource coverage; they never contact GitHub. MCP tools are
`list_issues`, `search_issues`, `get_issue`, `get_catalog`, `get_project`,
`find_duplicate_candidates`, and `get_sync_status`. HTTP provides `/health`,
`/v1/search`, `/v1/issues`, `/v1/issues/{owner}/{repo}/{number}`, `/v1/catalog`, `/v1/projects`,
`/v1/candidates`, `/v1/status`, `/snapshots/latest`, and `/snapshots/{filename}`.
HTTP requires bearer authentication outside loopback. Stdio MCP uses local filesystem
access. The HTTP server uses request limits/timeouts and graceful shutdown.

## Snapshot publication and acquisition

Snapshots use SQLite `VACUUM INTO` to produce a standalone database including FTS
and metadata from one SQLite read snapshot. Derive the manifest from the exported
database so concurrent collection cannot mix manifest and database generations.
Remove conditional caches, local credential identity and private kind inventory
from the export, then vacuum away deleted payload bytes. Keep source collector
state intact. Serialize publication through the single scheduled collector job.
Close and validate the output, fsync it, compute SHA-256, then install it with a unique
immutable filename. Finally atomically replace and fsync `latest.json`. The manifest
contains filename, checksum, schema and collection versions, generation, scope,
collection time and enrichment time. Publication and acquisition reject unsupported
collection versions; missing manifest versions default to legacy version 1.
Failed publication leaves the previous pointer intact. Versioned files are retained
until explicitly removed by operators; automatic retention is outside this version.
Acquisition currently bounds database downloads to 2 GiB.

Acquisition reads a local or HTTP manifest once, validates its fields and freshness,
copies/downloads its exact immutable file, verifies its checksum and embedded metadata,
and atomically installs a private destination. An optional maximum enrichment age
checks metadata freshness separately from issue/comment collection age. It rejects
scope/schema/collection-version mismatch and
unsafe filenames. A failed acquisition leaves an existing destination intact. A
published snapshot is never opened for writes. Copying a live WAL file is unsupported.

## WakeCI

Keep collector state outside WakeCI build workspaces, for example
`/home/client/gh-mirror/`. A scheduled collector job runs with concurrency one and
publishes after success. A gh-tidy build acquires one snapshot into
`${WAKE_BUILD_WORKSPACE}`, validates a configured maximum age, and pins it for its
whole batch. Record generation/checksum in the triage report. Before applying changes,
gh-tidy refreshes affected upstream tickets and supporting evidence.

Missing or stale snapshots fail explicitly; consumers never bootstrap the corpus
as a fallback. Bootstrap has a separate, longer timeout. Multiple CI hosts can use
the authenticated snapshot endpoint or private object storage with the same immutable
file and manifest contract. WakeCI's artifact redaction is not a binary-preserving
database transport. Keep reports/manifest metadata as artifacts, not SQLite files.

## Validation

`make check` performs formatting, vet, build, race-enabled tests, and lint, in order.
Fixtures cover bootstrap and delta sync, edited/deleted comments, closed history,
unused/removed labels, native fields and nested pagination, active/archived project
memberships, request counts, failure rollback, collector exclusion, complete local retrieval,
REST/MCP parity, snapshot isolation, failed publication, checksum/freshness/scope errors,
and acquisition across local and HTTP transports. Tests do not access live GitHub.
Concurrency tests verify worker bounds, a shared budget, coordinated retry waits,
queued cancellation, and unchanged bootstrap request counts. Recovery tests reopen
databases after interrupted pages/batches and budget exhaustion, verify that the old
generation remains visible, and check original watermarks and session invalidation.

Webhooks, attachment downloads, GitHub App token minting, object-storage
upload clients, and automatic snapshot retention are future extensions. Existing
installation/PAT credentials can be supplied through the token environment variable.

## Offline embeddings

The binary bundles pinned FP32 MiniLM L6 weights and its WordPiece tokenizer for
standalone Go CPU inference. Semantic search uses exact filtered cosine scans;
hybrid search combines independent lexical and semantic ranks. Titles, bodies,
labels and collected comments produce bounded overlapping passages, with mean
pooling and L2 normalization. SQLite snapshots contain the compatible vectors.

`index` operates only on local documents. Indexing skips unchanged documents,
reuses identical passages after edits and durably checkpoints finished vectors.
Changing the embedding fingerprint rebuilds this derived index without GitHub
collection or invalidating compatible collection checkpoints.

Optional `embedding.backend: lemonade-vulkan` starts a dedicated authenticated
loopback process using Lemonade's installed Vulkan runtime. It exports the exact
bundled FP32 tensors to GGUF locally and sends bundled tokenizer IDs directly.
Inference is offline after backend installation. GPU batching and CPU fallback
are bounded; missing devices, incompatible vectors and runtime failures select
the same bundled CPU model with an explicit reason. Validate full GPU offload,
reference vectors, full-length passages, response identity, dimensions and unit
norm before accepting GPU vectors. Backend selection never changes model identity.
CPU/Vulkan results must have cosine >=0.9999 and coordinate difference <=0.001.

The configured encoder is shared by CLI, REST and MCP query paths. Runtime progress
reports the actual backend/device, batch size, vector throughput and fallback
reason while preserving document progress and resume checkpoints. `model --check`
verifies the selected backend without opening or modifying the mirror.

## Repository resource options

`repository_options` maps exact configured repository names to eleven booleans:
`issues`, `pull_requests`, `issue_comments`, `pull_request_comments`,
`pull_request_review_comments`, `labels`, `milestones`, `issue_types`, `fields`,
`relationships`, and `projects`. Explicit entries default omitted switches to false;
missing entries preserve legacy global defaults. Inline review comments default
false. `scope` prints effective options; committed status records them. Ticket
payload metadata remains intact regardless of catalog switches. At least one ticket
kind must be enabled. Shared owner catalogs use the union of enabled resources.

Keep minimal private ticket number/kind observations to filter bulk comment streams
without per-excluded-ticket fetches. Review comment IDs are namespaced separately
from discussion IDs; raw payload IDs remain unchanged. Strip kind inventory from
exports. Ticket/comment changes reconcile only the affected repository; metadata
changes rehydrate without forcing full inventories. Persist resolved options only
with successful collection. Equivalent explicit defaults preserve legacy pending
fingerprints. Different resource selections participate in pending session identity.

## Listing and compatibility

CLI `list`, REST `/v1/issues` and MCP `list_issues` enumerate raw ticket payloads,
fields and observations with repository/state/label/type/kind/project filters,
ordered by repository and ticket number. Comments are retrieved through `get`.
Opaque keyset cursors bind filters and committed generation; reject changed filters
or generations. Search also supports kind and owner/project-number filters,
including archived memberships and enterprise/user project URLs.

Credential/upstream identities are opaque hashes retained only in collector
metadata. Changes or missing identity on an existing mirror require an atomic,
resumable full rebuild with cleared conditional cache. Same-token permission changes
still use explicit or periodic reconciliation. GraphQL observations are validated
before checkpointing; refetch invalid legacy responses without discarding good work.

GitHub Actions checks formatting, vet, build, race tests and lint. Tagged releases
require a version matching monova and package CGO-free Linux/macOS binaries for
amd64 and arm64 with SHA-256 checksums. Package the executable, README and bundled model notices/license.

## Query and result expansion

Keep SQLite as the default retrieval engine and preserve literal OR search. Add
explicit any/all/phrase modes, prefix matching, exclusions, source selection and
composable ticket predicates. All-word matching spans selected documents belonging
to one ticket; phrase matching stays within one document. No query exposes raw SQL
or FTS syntax. Reject excessive terms instead of silently dropping them.

Search and list pages bind continuation to a committed generation and effective
query, with deterministic ties. Support relevance, number, creation and update
ordering, summary/full projections, optional totals and bounded label/type/project
facets. Search results expose metadata, ranked source evidence and score semantics.
Provide bounded comments and catalogs, batched ticket reads, explicit coverage
warnings, typed REST errors and documented MCP output envelopes. Preserve legacy
full reads as explicit or existing compatibility paths. Improve duplicate term
selection using local document frequency, technical identifiers and optional seed
labels/comments; evaluate candidate recall and search precision on versioned local
fixtures. Bundled semantic/hybrid search is explicitly required; use the same
evaluation format to compare retrieval quality.

## Bundled offline vectors

Embed the pinned Apache-2.0 all-MiniLM-L6-v2 safetensors, WordPiece vocabulary,
configuration, checksum provenance and license in the executable. Use pinned
rembed Go/assembly inference, FP32 mean pooling and L2 normalization, with 384
coordinates. Extract verified bundled bytes to a private XDG cache directory;
load only its absolute existing path, never a remote model ID. No model download,
model service, CGO, shared library or GPU/NPU driver is required at runtime.
The model research and hardware measurements live in semantic-model-research.md.

SQL schema 2 adds semantic document state and little-endian FP32 passage vectors.
Writers migrate schema 1 locally; readers accept both schemas without writing.
Collection compatibility remains version 1. Keep the model/tokenizer, inference,
pooling, precision and chunking fingerprint separate from collection identity.
Schema-2 snapshots require a compatible new reader binary.

Split title/body and label/comment/review sources into passages of at most 256
WordPiece tokens including framing, with a 2,048-rune character cap, 32-rune overlap and original byte offsets.
Preserve long-document tails. Index with bounded independent CPU workers, default
four, and FULL synchronous SQLite commits. Resume completed passages after errors,
interrupts and process restarts. Serialize same-store write transactions through a
cancellable queue before acquiring pooled connections; keep readers and CPU inference
concurrent and preserve SQLite locking against independent collectors. Text edits
invalidate completeness but keep reusable passages locally; metadata-only updates
retain vectors. Match reusable text exactly
within its field and model. Guard checkpoint commits against changed source text
and model identity. Deletions cascade. Exports remove incomplete/obsolete passages
and compact the file; completed vectors travel in the SQLite snapshot and its
manifest records embedding fingerprint, dimension and index generation.

`index` performs local indexing only. `sync` indexes after the collection commit and
before publication by default, with `--index=false` available and separate
`--index-workers`. When omitted, CPU workers inherit the effective sync worker
setting; standalone `index` inherits configured workers unless its --workers flag
overrides them. Keep a persistent worker pool fed through a bounded document queue
across 32-row keyset reads, without waiting for a page's slowest document. Rescan
after finishing a pass to catch remaining work. Serialize active-worker snapshots
and label GitHub/CPU workers separately. These scheduling changes preserve embedding
compatibility and durable passages. A failure during indexing retains committed collection and
completed vectors; rerun `index` to finish without new upstream requests. Progress
uses stderr, reports known document totals, rate, ETA and active CPU workers, and
stops on completion/cancellation/failure.

CLI, REST and MCP default search and seed-based candidate retrieval to hybrid,
with explicit lexical, semantic and hybrid overrides. Retrieval supports agent
ticket evaluation as well as duplicate discovery: preserve behavior descriptions,
decisions, workarounds, fixes and related history through summaries and bounded
source-linked evidence. Search all collected titles, bodies, attached labels,
discussion comments and review comments unless source scopes are supplied. Include
open and closed tickets unless state filters are supplied.
Semantic queries average normalized query passage embeddings and use exact cosine
scanning over filtered vectors, selecting the best passage per ticket. Semantic duplicate
candidates reuse weighted stored seed vectors: title 4, body 1, labels 1.5 and
bounded recent comments 0.5, averaging passages within each source family before
normalizing. Hybrid combines independent lexical and semantic ticket ranks with
reciprocal rank fusion constant 60. Preserve exact ticket filters, source scopes,
exclusions, counts, facets, projections, evidence and deterministic pagination.
Report score meanings and component scores; semantic/hybrid default to descending
relevance. Evidence includes field, source, byte offsets, cosine similarity and
stored comment/review context. Candidate seeds are excluded from results.

For an omitted engine, unavailable, incomplete or incompatible selected indexing
coverage selects lexical retrieval with a structured search_engine_fallback warning
and indexing instructions. Preserve the original lexical candidate term selection.
Report the actual engine and its score meaning. Do not fall back for invalid queries,
inference errors, cancellation, database failures or stale cursors. Explicit
semantic/hybrid queries still return 503 semantic_index_unavailable. Never omit
pending tickets by querying partial vector coverage. Default queries with all-word,
phrase or prefix controls select lexical with a search_engine_selected explanation;
explicit semantic engines reject those controls. Engine changes during continuation
return a stale-cursor conflict and require restarting the search. Bound ordinary
queries to 16 KiB, 16 semantic passages and 512 hybrid literal terms. Bind semantic
cursors to collection generation, vector generation, model and effective options.
Status reports durable document/chunk coverage and compatibility. Query tools remain
read-only and never initiate indexing or GitHub requests.
