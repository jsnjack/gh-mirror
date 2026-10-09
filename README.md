# gh-mirror

gh-mirror keeps GitHub issues, conversation comments, labels, fields, and projects
in a searchable local SQLite database. Collect repositories once, update them
incrementally, and reuse the data for issue triage, reports, automation, or AI
tools without repeatedly fetching the same records from GitHub.

A single Go executable provides command-line queries, a REST API, MCP tools, and
portable database snapshots. It runs without an external database or search
service. Queries work offline; only collection needs GitHub access. All upstream
operations are read-only.

## Build

Build with Go 1.26.4 or newer and `monova` installed:

```sh
make build
```

The binary is `bin/gh-mirror`. Builds disable CGO. Run it directly, or install it
in a directory on your `PATH` to use the commands below:

```sh
mkdir -p ~/.local/bin
install -m 755 bin/gh-mirror ~/.local/bin/gh-mirror
gh-mirror --help
```

## Configure

Copy [examples/config.json](examples/config.json) into
`~/.config/gh-mirror/config.json` and replace `owner/repository` with the
repositories you want to collect:

```sh
mkdir -p ~/.config/gh-mirror
cp examples/config.json ~/.config/gh-mirror/config.json
```

The configuration below shows the default collection settings. Omitted settings
retain their defaults:

```json
{
  "repositories": ["owner/repository"],
  "fields": true,
  "projects": true,
  "max_requests": 3000,
  "overlap": "5m",
  "enrichment_interval": "1h",
  "reconcile_interval": "24h"
}
```

The default database and snapshots are under `~/.local/share/gh-mirror/`.
Set `database` and `snapshot_dir` in the configuration to use other locations.
XDG environment variables are respected. `--config` overrides the settings file;
`--db` overrides the database for any command.

Provide your GitHub credential through `GITHUB_TOKEN` in the shell or scheduler
environment. HTTP authentication uses `GH_MIRROR_API_TOKEN`. Configuration can
change those environment variable names; credentials are not stored in the mirror.

The GitHub credential needs access to the configured repositories, issue data,
organization issue types/fields, and owner projects. The available resources
depend on the token's visibility and GitHub features. Enabled features that return
403/404 or GraphQL errors fail collection. Set `fields` or `projects` to `false`
explicitly when those features should be excluded; status records that exclusion.
Personal repositories have no organization issue fields. User-owned Projects REST
endpoints require a compatible classic PAT; GitHub's current documentation excludes
fine-grained and installation tokens for those endpoints.

## Create and update a mirror

The first sync creates the database and collects the configured repositories.
Later runs update the same database incrementally:

```sh
gh-mirror sync
gh-mirror status
```

`sync` runs once and exits. It publishes a portable snapshot after successful
collection. Use `sync --publish=false` if you only need the local database.
`status` reports the generation, repository scope, coverage, counts, and collection
and enrichment timestamps. A failed collection rolls back its data, indexes, and
checkpoints together, leaving the previous successful generation available.

Progress appears on stderr immediately, without `--debug`. In a terminal it
refreshes a live display with the current phase, repository, pages and records,
elapsed time, HTTP request budget, conditional cache hits, and GitHub's remaining
rate allowance when returned. Indexing and metadata hydration show a progress bar
once their record totals are known:

```text
gh-mirror sync | elapsed 12s
⠹ Hydrating issue metadata | [======--------------] 100/300
Received: 300 issues, 840 comments | repository 1/1
API: 19/3000 requests | 0 cached | GitHub remaining: 4981
```

Listings use a spinner and counts because their totals are not known in advance.
Progress continues during network requests, retry waits, and snapshot publication.
Success, failure, and cancellation have explicit final status messages. Redirected
stderr and `--debug` use plain phase updates with periodic status lines instead of
terminal animation. Stdout remains JSON; use `sync --quiet` to suppress progress.

Schedule sync with cron, a systemd timer, or your CI scheduler. For example, this
cron entry updates every 15 minutes without exporting a snapshot:

```cron
*/15 * * * * /absolute/path/gh-mirror --config /absolute/path/config.json sync --publish=false
```

Supply `GITHUB_TOKEN` to the scheduler and keep the database on persistent storage.
Run one collector at a time. `serve` and `mcp` read the database; they do not
schedule updates themselves.

Issue and comment changes are fetched on each sync. Catalogs, native fields,
relationships, and projects are refreshed every `enrichment_interval` (default
one hour). Full issue/comment inventories are reconciled every
`reconcile_interval` (default 24 hours) to remove deleted or transferred records.
These separate refreshes cover changes GitHub does not reliably expose through
issue timestamps.

Force complete inventories and enrichment immediately with:

```sh
gh-mirror sync --full
```

Run this after changing credentials or permissions. Changing the repository list
or upstream API host automatically forces complete collection and removes data
outside the new scope.

## Query locally

Search issue titles, bodies, and comments:

```sh
gh-mirror search 'socket timeout'
gh-mirror search 'socket timeout' --repo owner/repository --state closed --label bug --limit 20
gh-mirror search 'socket timeout' --type Bug
```

Queries return JSON on stdout. Search treats words literally, joins them with OR,
and returns the best matching issue or comment per ticket, with a source URL,
snippet, and text relevance score. Filters select repository, state, label, and
native issue type. Results include mirror status so consumers can check freshness.

Read a complete issue with its comments and collected metadata, or retrieve
possible duplicates:

```sh
gh-mirror get --repo owner/repository --number 123
gh-mirror candidates --repo owner/repository --number 123 --limit 30
```

Candidates use the seed issue's title and body to find similar text in the same
repository. They include closed issues and exclude the seed. Scores measure text
relevance, not duplicate probability; review the returned evidence before acting.

Inspect labels, milestones, native issue types/fields, or project data:

```sh
gh-mirror catalog --kind labels --scope owner/repository
gh-mirror catalog --kind issue_fields --scope owner
gh-mirror catalog --kind project_items --scope owner/1
```

| Catalog kinds | Scope |
| --- | --- |
| `labels`, `milestones` | `owner/repository` |
| `issue_types`, `issue_fields`, `projects` | `owner` |
| `project_fields`, `project_items` | `owner/project-number` |

Use `gh-mirror --db /path/to/mirror.sqlite ...` to query another database, including
an acquired snapshot. Local queries need no GitHub credential.

## Run the REST API

Start a server against the existing database:

```sh
gh-mirror serve --listen 127.0.0.1:8787
```

Query it from another terminal:

```sh
curl 'http://127.0.0.1:8787/v1/status'
curl 'http://127.0.0.1:8787/v1/search?q=socket+timeout&repo=owner/repository'
curl 'http://127.0.0.1:8787/v1/issues/owner/repository/123'
```

| GET endpoint | Purpose |
| --- | --- |
| `/health` | Health check |
| `/v1/status` | Generation, scope, coverage, and freshness |
| `/v1/search?q=...` | Search; supports `repo`, `state`, `label`, `type`, and `limit` |
| `/v1/issues/{owner}/{repo}/{number}` | Issue, comments, and metadata |
| `/v1/catalog?kind=...&scope=...` | Catalog data |
| `/v1/projects?owner=...&number=...` | Project with fields and items |
| `/v1/candidates?repo=...&number=...&limit=...` | Possible duplicate issues |
| `/snapshots/latest` | Latest snapshot manifest |
| `/snapshots/{filename}` | Immutable snapshot file |

Non-loopback listeners require `GH_MIRROR_API_TOKEN`; requests then use
`Authorization: Bearer <token>`. Setting the token enables authentication for all
endpoints even on loopback. Use a TLS proxy when exposing the service remotely.

## Run MCP

For an MCP client using stdio, configure the executable and database:

```json
{
  "mcpServers": {
    "gh-mirror": {
      "command": "/absolute/path/to/gh-mirror",
      "args": ["--db", "/absolute/path/to/mirror.sqlite", "mcp"]
    }
  }
}
```

The stdio command is `gh-mirror --db /path/to/mirror.sqlite mcp`; stdout contains
only protocol traffic. The HTTP server also provides Streamable HTTP MCP at
`http://127.0.0.1:8787/mcp`, with the same bearer authentication as REST.

The six read-only tools are `search_issues`, `get_issue`, `get_catalog`,
`get_project`, `find_duplicate_candidates`, and `get_sync_status`.

## Share a snapshot

Use snapshots to give another machine, CI job, or batch process a private copy of
one complete generation. `sync` publishes a snapshot by default; publish the last
successful collection separately with:

```sh
gh-mirror snapshot
```

The publisher exports a standalone SQLite file including its search index, assigns
an immutable filename, and atomically updates `latest.json`. Use this export rather
than copying the main file of a live SQLite database with WAL sidecars.

Acquire from a shared directory:

```sh
gh-mirror acquire \
  --source /srv/gh-mirror/snapshots/latest.json \
  --dest ./mirror.sqlite \
  --repo owner/repository --max-age 2h > mirror-manifest.json

gh-mirror --db ./mirror.sqlite search 'socket timeout'
```

Or acquire through the REST server behind an HTTPS proxy:

```sh
gh-mirror acquire --source https://mirror.example/snapshots/latest \
  --dest ./mirror.sqlite --repo owner/repository --max-age 2h
```

Acquisition resolves the manifest once, checks age and exact repository scope,
verifies SHA-256 and embedded metadata, then atomically installs the private file.
For multiple repositories, repeat `--repo` or use a comma-separated list. Failed
acquisition preserves the previous destination. The destination must have no live
WAL sidecars; the current download limit is 2 GiB.

Keep the database pinned for the whole batch and retain the manifest to record its
generation and checksum. Snapshot files remain until you remove them. Serialize
publication and retain generations long enough for in-flight acquisitions to finish.

## Collection scope and efficiency

The mirror includes accessible open and closed issues, ordinary PR conversation
records, and all conversation comments. Original REST JSON preserves bodies,
authors, labels, multiple assignees, state reasons, native types, and other returned
fields. Catalogs include unused labels and available fields. Project collection
includes active, archived, and draft items with field values.

PR reviews, inline review comments, Discussions, attachment binaries, and historical
deleted text are outside this version. The mirror reflects the credential's visible
scope and the last successful collection, rather than live GitHub state.

Bootstrap fetches issues and repository-wide comments in pages of 100. Native
fields and relationships use GraphQL batches of 50 tickets across repositories.
Owner catalogs and projects are collected once per owner. Project inventories are
checked against the bulk listing, with individual requests only for omitted items.

Between scheduled enrichment and full reconciliation, an unchanged repository
requires two requests: one issue delta listing and one comment delta listing.
Polling comments separately catches edits to comments on older issues. The overlap
window avoids gaps; unchanged overlapping issues do not trigger extra hydration.
Changed or new issues are hydrated in batches. Reusable listings use conditional
requests; a 304 still counts as a request.

Each sync reports its actual HTTP attempt count. Pagination, GraphQL, and retries
share `max_requests`; exhausting the budget or encountering a long rate-limit wait
fails collection so a later run can retry. Longer refresh intervals reduce requests
at the cost of older field, project, or deletion information.

## Development and diagnostics

Run `make check` for formatting, vetting, build, race tests, and linting. Checks
require `goimports`, `golangci-lint`, and `monova`; the Makefile prints installation
commands for missing tools. The implementation specification is in
[docs/spec.md](docs/spec.md).

`--debug` adds request diagnostics to stderr alongside plain sync progress.
`--trace` independently writes detailed
request/count/status metadata to `gh-mirror.log` in the temporary directory
(normally `/tmp`), truncated at startup.
Credentials and ticket bodies are excluded from diagnostic logs.

## API references

Collection uses GitHub's [repository issue listing](https://docs.github.com/en/rest/issues/issues#list-repository-issues),
[repository comment listing](https://docs.github.com/en/rest/issues/comments#list-issue-comments-for-a-repository),
[native issue GraphQL schema](https://docs.github.com/en/graphql/reference/issues),
[Projects](https://docs.github.com/en/rest/projects/projects),
[project fields](https://docs.github.com/en/rest/projects/fields), and
[project items](https://docs.github.com/en/rest/projects/items). The REST version is
`2026-03-10`. Feature availability on GitHub Enterprise Server may differ; unsupported
enabled features fail rather than being reported as empty complete catalogs.
