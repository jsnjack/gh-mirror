# gh-mirror

Read [universal conventions](AGENTS.universal.md) and [Go conventions](AGENTS.go.md).
The specification is in [docs/spec.md](docs/spec.md). Run `make build` after runtime
changes and `make check` before reporting completion. Do not install missing tools,
commit, push, or change another repository without user authorization.

`cmd/` parses Cobra commands and wires dependencies. `internal/config` loads JSON
configuration and resolves XDG paths. `internal/diagnostics` manages stderr and trace
logging. `internal/github` performs bounded read-only upstream requests and pagination.
`internal/collect` synchronizes configured resources. `internal/store` owns the SQLite
schema, transactions, FTS5 index, and queries. `internal/snapshot` publishes and acquires
immutable databases. `internal/service` shares local queries between REST and MCP.

GitHub remains authoritative. Never issue upstream mutations. Preserve raw payloads,
all accessible comments, native issue types, multiple assignees, unused labels, and
separate organization issue fields from project fields. Verify bulk project items against the GraphQL inventory including both archived
states; recover missing projected records by stable ID. Never treat failed pagination,
inaccessible resources, or unsupported fields as an empty successful collection.
Only successful collection transactions advance checkpoints. Poll comments separately
from issues. Bootstrap uses 100-record REST pages and 50-node GraphQL batches shared
across repositories. Hydrate only changed tickets on deltas; reconcile catalogs,
projects, fields and relationships on the configured enrichment interval. Expose
that timestamp separately. Never retain non-reusable since-response cache entries. Full reconciliation removes stale issues, comments, and memberships.

Readers open existing databases without migrations or writes. Publish standalone
SQLite exports, never copies of live WAL databases. Complete and validate the export
before atomically replacing `latest.json`. Acquisition resolves the manifest once,
verifies checksum, scope, schema and freshness, and installs a private local copy.
Credentials come from named environment variables and never enter the database,
stdout, request bodies, or trace logs. HTTP listeners outside loopback require an
API bearer token. Read tools never trigger collection or expose arbitrary SQL.

Dependencies are justified by the accepted design: Cobra is required by standards;
modernc.org/sqlite provides embedded SQLite/FTS5 without CGO; the official MCP SDK
provides protocol and transport handling. Tests use local HTTP fixtures and temporary
databases. No live GitHub writes or production WakeCI changes belong in validation.
