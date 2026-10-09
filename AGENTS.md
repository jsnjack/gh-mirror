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

Workers share concurrency permits, budget reservations, retry pauses, and serialized
progress callbacks. Parallelize independent listings and hydration batches; follow
dependent pagination sequentially. Apply hydration results serially and join workers
before ending the collection transaction. One worker must support serial collection.
Save completed responses in the private `<database>.sync.sqlite` checkpoint with
FULL synchronous durability, including delta pages. This temporary resume store is
separate from the reusable conditional cache. Keep the original collection-start
watermark on resume. Fingerprint scope/settings/credential identity without storing
credentials; worker/budget changes preserve pending work. Initialize and clean up
sessions under the collector lock. Never discard staged work before the mirror commits
or delete another collector's session. Snapshot exports exclude pending work.

Readers open existing databases without migrations or writes. Publish standalone
SQLite exports, never copies of live WAL databases. Complete and validate the export
before atomically replacing `latest.json`. Acquisition resolves the manifest once,
verifies checksum, scope, schema and freshness, and installs a private local copy.
Credentials come from named environment variables and never enter the database,
stdout, request bodies, or trace logs. HTTP listeners outside loopback require an
API bearer token. Read tools never trigger collection or expose arbitrary SQL.

Sync progress starts before opening the database and uses stderr, preserving JSON
on stdout. Animate terminal output; use plain phase updates and periodic heartbeats
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

Dependencies are justified by the accepted design: Cobra is required by standards;
modernc.org/sqlite provides embedded SQLite/FTS5 without CGO; the official MCP SDK
provides protocol and transport handling. The existing go-isatty dependency detects
terminal writers without adding a UI framework. Tests use local HTTP fixtures and temporary
databases. No live GitHub writes or production WakeCI changes belong in validation.
