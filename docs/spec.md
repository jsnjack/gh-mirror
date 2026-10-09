# gh-mirror specification

## Purpose

gh-mirror collects GitHub ticket data into a local SQLite database for gh-tidy,
personal tools, and small organizations. GitHub remains the source of truth.
Duplicate candidate discovery uses local indexes; ticket changes remain the
responsibility of gh-tidy and its existing review, refresh, and recovery gates.

## Deployment

The application is a Go executable built without CGO. It requires no external
database, search engine, queue, or embedding model. Cobra follows workspace
standards. modernc.org/sqlite supplies SQLite and FTS5 without platform-specific
C libraries. The official Go MCP SDK handles stdio and Streamable HTTP.

The CLI supports `sync`, `search`, `get`, `candidates`, `catalog`, `status`, `snapshot`,
`acquire`, `serve`, and `mcp`. All commands inherit `--config`/`-c`, `--debug`/`-d`,
and `--trace`. The root supports `--version`. Configuration is JSON under the XDG
configuration directory. Durable data uses the XDG data directory. Tokens are
read from named environment variables, never configuration values or databases.

## Data contract

Configured repositories include open and closed issues and ordinary PR conversation
records, distinguished by kind. Preserve upstream JSON, complete Markdown bodies,
authors, timestamps, URLs, state reasons, assignees, labels, milestones, and native
issue types. PR reviews, review comments, diffs, and attachment binaries are outside
this version; their conversation comments are included. Preserve attachment links.

Collect all accessible repository comments with stable IDs. Catalogs include unused
repository labels and milestones, organization issue types, and organization issue
field definitions/options. Native issue values are collected in batched GraphQL
node queries with pagination. Project catalogs include accessible owner projects,
their field definitions, active/archived items, draft items, memberships, and field
values. Project data can include references to repositories outside the configured
ticket corpus; those references do not imply issue/comment coverage there.

Organization issue fields and project fields remain separate. Extra issue GraphQL
metadata includes parent, sub-issues, blocked-by, blocking, and project memberships.
Every nested connection must be paginated. Unavailable organization features can be
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

New and changed tickets receive batched field/relationship hydration. Unchanged
overlapping REST payloads do not trigger hydration. Native field values, relationships,
project membership, and owner catalogs are also reconciled independently of issue
update timestamps every `enrichment_interval` (default one hour). The last complete
enrichment timestamp is exposed separately from issue/comment collection time.
Project item collection inventories both `ARCHIVED` and `NOT_ARCHIVED` states through
GraphQL and requests every configured field through bulk REST. Missing inventory
items receive a complete REST fetch by stable ID; failed recovery rolls back.
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
nested overflow requires 20 bootstrap requests. Exact conditional responses retain
ETags and Link headers for reusable listing/catalog URLs. Delta URLs with changing
`since` watermarks are not persisted in the HTTP cache; conditional requests still count against the request budget.
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

Sync reports progress on stderr by default, starting before database setup. A
terminal receives a refreshed display of phase, repository, listing counts,
elapsed time, request usage, conditional cache hits, and rate-limit information.
It also reports active/configured workers and saved responses reused on resume.
Known indexing/hydration totals have progress bars; listings with unknown totals
have a spinner. Explicit callbacks carry activity from the collector and client
without additional upstream calls. Status refreshes during slow requests, retries,
and snapshot publication. Nonterminal/debug output uses plain phase updates and
periodic heartbeats. `sync --quiet` disables progress; stdout remains JSON.
Success, failure, and cancellation stop refreshes and produce a final status.

## Retrieval

FTS5 indexes issue titles/bodies and individual comments. Results return issue
identity, source URL, matched snippet, ranking, timestamps, and collection status.
Literal user words form an OR query for candidate recall; raw FTS/SQL is not exposed.
Filters include repository, state, label, and native issue type. Candidate retrieval
derives distinctive terms from a stored ticket, excludes that ticket, includes closed
history, and returns ranked suggestions. Scores are retrieval scores, not duplicate
probabilities. No automatic duplicate adjudication or closure belongs in gh-mirror.

`get` returns the complete stored issue, comments, native field values, relationships,
and project memberships. REST and MCP call the same query functions. Reads report the
database generation and resource coverage; they never contact GitHub. MCP tools are
`search_issues`, `get_issue`, `get_catalog`, `get_project`,
`find_duplicate_candidates`, and `get_sync_status`. HTTP provides `/health`,
`/v1/search`, `/v1/issues/{owner}/{repo}/{number}`, `/v1/catalog`, `/v1/projects`,
`/v1/candidates`, `/v1/status`, `/snapshots/latest`, and `/snapshots/{filename}`.
HTTP requires bearer authentication outside loopback. Stdio MCP uses local filesystem
access. The HTTP server uses request limits/timeouts and graceful shutdown.

## Snapshot publication and acquisition

Snapshots use SQLite `VACUUM INTO` to produce a standalone database including FTS
and metadata from one SQLite read snapshot. Derive the manifest from the exported
database so concurrent collection cannot mix manifest and database generations.
Serialize publication through the single scheduled collector job.
Close and validate the output, fsync it, compute SHA-256, then install it with a unique
immutable filename. Finally atomically replace and fsync `latest.json`. The manifest
contains filename, checksum, schema version, generation, scope, and collection time.
Failed publication leaves the previous pointer intact. Versioned files are retained
until explicitly removed by operators; automatic retention is outside this version.
Acquisition currently bounds database downloads to 2 GiB.

Acquisition reads a local or HTTP manifest once, validates its fields and freshness,
copies/downloads its exact immutable file, verifies its checksum and embedded metadata,
and atomically installs a private destination. It rejects scope/schema mismatch and
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
unused/removed labels, native fields and nested pagination, archived/draft project
items, request counts, failure rollback, collector exclusion, complete local retrieval,
REST/MCP parity, snapshot isolation, failed publication, checksum/freshness/scope errors,
and acquisition across local and HTTP transports. Tests do not access live GitHub.
Concurrency tests verify worker bounds, a shared budget, coordinated retry waits,
queued cancellation, and unchanged bootstrap request counts. Recovery tests reopen
databases after interrupted pages/batches and budget exhaustion, verify that the old
generation remains visible, and check original watermarks and session invalidation.

Embeddings, webhooks, attachment downloads, GitHub App token minting, object-storage
upload clients, and automatic snapshot retention are future extensions. Existing
installation/PAT credentials can be supplied through the token environment variable.
