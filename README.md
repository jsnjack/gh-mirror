# gh-mirror

gh-mirror keeps GitHub issues, conversation comments, labels, native issue fields,
project catalogs, and ticket project memberships
in a searchable local SQLite database. Collect repositories once, update them
incrementally, and reuse the data for issue triage, reports, automation, or AI
tools without repeatedly fetching the same records from GitHub.

A single Go executable provides command-line queries, a REST API, MCP tools, and
portable database snapshots. It runs without an external database or search
service. Queries work offline; only collection needs GitHub access. All upstream
operations are read-only.

## Install

Download an archive for Linux or macOS, on amd64 or arm64, from
[GitHub releases](https://github.com/jsnjack/gh-mirror/releases). Each archive
contains the executable and README; release assets include `checksums.txt`.
Extract it and install the executable in your `PATH`. Go is unnecessary at runtime.

To build from source, use Go 1.26.4 or newer and `monova` installed:

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

Commands use readable text when stdout is a terminal and JSON when redirected or
piped. Search and candidate results show ranked tickets, labels and evidence;
`evaluate` shows a metrics table. Select a format explicitly with `--format`:

```sh
gh-mirror search 'socket timeout' --format text
gh-mirror candidates --repo owner/repository --number 123 --format text
gh-mirror status --format json
gh-mirror search 'socket timeout' | jq '.matches'
```

`--format json` preserves the complete structured output. Text results use compact
previews; REST and MCP always retain their structured contracts.

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
  "workers": 4,
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
403/404 or GraphQL errors fail collection. Disable unavailable resources in
`repository_options`; status records the resolved scope.
Personal repositories have no organization issue fields. User-owned Projects REST
endpoints require a compatible classic PAT; GitHub's current documentation excludes
fine-grained and installation tokens for those endpoints.
`projects` collects available project catalogs and ticket memberships. It does not
fetch complete project items, draft cards, or project field definitions/values.

## Select resources per repository

Add an explicit `repository_options` entry for each repository you want to control.
Every switch is a boolean; omitted switches in an explicit entry are **false**.
Repositories without an entry retain the defaults above, including issue and PR
conversation comments; inline review comments default to false.

```json
{
  "repositories": ["owner/repository", "owner/code"],
  "repository_options": {
    "owner/repository": {
      "issues": true,
      "pull_requests": true,
      "issue_comments": true,
      "pull_request_comments": true,
      "pull_request_review_comments": false,
      "labels": true,
      "milestones": true,
      "issue_types": true,
      "fields": true,
      "relationships": true,
      "projects": true
    },
    "owner/code": {
      "issues": false,
      "pull_requests": true,
      "issue_comments": false,
      "pull_request_comments": true,
      "pull_request_review_comments": true,
      "labels": false,
      "milestones": false,
      "issue_types": false,
      "fields": false,
      "relationships": false,
      "projects": false
    }
  }
}
```

| Switch | Collected resource |
| --- | --- |
| `issues` | Open and closed issues and their raw ticket payloads |
| `pull_requests` | Open and closed PR ticket records from the issue listing |
| `issue_comments` | Issue discussion comments |
| `pull_request_comments` | PR discussion comments |
| `pull_request_review_comments` | Inline PR review comments, including diff context and replies |
| `labels` | Available label catalog, including unused labels |
| `milestones` | Available milestone catalog, including closed milestones |
| `issue_types` | Organization issue type catalog |
| `fields` | Native issue field definitions/options and ticket values |
| `relationships` | Parent, sub-issues, blocked-by and blocking relationships |
| `projects` | Owner project catalog and active/archived ticket memberships |

Comments require their corresponding ticket kind. Issue types, native fields and
issue relationships apply when issues are enabled. Shared owner catalogs are
collected once when any configured repository requests them. Each repository must
enable issues or pull requests. The raw ticket response always preserves titles,
bodies, state, authors, assignees, attached labels, milestones and issue type;
turning off a catalog does not remove those fields or their label search matches.

PR records currently use the issue representation; full PR details, commits, file
patches, check runs, approval reviews, reactions and attachment binaries are outside
this scope. Full project cards, draft items and project custom fields are excluded.

```sh
gh-mirror scope
```

`scope` shows effective options without contacting GitHub; `status` shows the
options used for the committed mirror. Ticket/comment scope changes reconcile the
affected repository. Catalog/metadata changes refresh its selected observations
without forcing full ticket listings. Interrupted changes preserve the previous
mirror and resume fetched work. Bulk comment streams use a private number/kind
inventory to avoid fetching each excluded comment parent.

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

`collection_version` records data compatibility separately from the application
release and SQLite schema. When an incompatible collection upgrade changes this
version, sync automatically rebuilds all configured repositories and metadata,
clears incompatible cached responses, and reports why a full sync is required.
The previous mirror stays available until the rebuild commits. Interrupted
rebuilds resume automatically, then later runs return to incremental updates.
Compatible code changes retain existing data and pending work. Existing mirrors
without this metadata are treated as version 1 and do not need an extra fetch.
Network errors and rate limits preserve progress; they do not trigger a full rebuild.

Sync uses four coordinated workers by default. Independent issue/comment listings
and advertised numbered pages and batches of ticket metadata run in parallel;
cursor-dependent pagination follows its next link in order. Set `workers` in the
configuration or override it for one run:

```sh
gh-mirror sync --workers 4
gh-mirror sync --workers 1
```

The allowed range is 1–16. Workers share one request budget and rate-limit pause.
Parallel fetching uses the same bulk requests as serial collection. Use one worker
when you need serial requests or encounter secondary rate limits.

Stop with Ctrl-C and rerun the same command to resume automatically. Completed
REST pages and GraphQL responses are committed immediately to
`<database>.sync.sqlite`, including nested pagination. In-flight requests may need
to be repeated. Saved responses cost no API requests on resume; the progress display
and JSON result (`resumed_responses`) report how many were reused. The original sync
start time is retained, so the next incremental update can catch changes made
during the interruption. After a long interruption, run sync again to catch up.

Keep the database directory on persistent storage, including the pending file and
its SQLite sidecars. This private staging data is removed after a successful commit
and is excluded from published snapshots. Readers continue seeing the last complete
generation until the resumed sync finishes. Worker-count and request-budget changes
preserve pending work; changes to scope, upstream, collection settings, full mode,
or credentials start a new session. To explicitly discard unfinished work, use:

```sh
gh-mirror sync --restart
```

Progress appears on stderr immediately, without `--debug`. In a terminal it
refreshes a live display with the current phase, repository, pages and records,
elapsed time, HTTP request budget, conditional cache hits, and GitHub's remaining
rate allowance when returned. Rows wrap to the terminal width, with GitHub quota
and resume counters on separate rows. Indexing and metadata hydration show a
progress bar, percentage, speed, and estimated time for the current phase once
their record totals and throughput are known:

```text
gh-mirror sync | elapsed 12s
⠹ Hydrating issue metadata
[======--------------] 100/300 (33%) | 25.0/s | phase ETA 8s
Received: 300 issues, 840 comments | repository 1/1
workers 4/4 active | API: 19/3000 requests | 0 cached
GitHub remaining: 4981
Resume: 8 resumed responses | 10 saved before this run
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

Changed credentials automatically require full reconciliation. Existing mirrors
without a recorded credential identity perform one full reconciliation on upgrade.
The identity is an opaque local hash, committed only on successful sync and excluded
from snapshots and status. Permission changes on an unchanged token still require
`--full` or scheduled reconciliation. Changing the repository list
or upstream API host automatically forces complete collection and removes data
outside the new scope.

## Offline semantic and hybrid search

The executable includes MiniLM L6 weights, its WordPiece tokenizer and model
configuration (about 91 MB of model assets). Inference runs in Go on the CPU.
The first use extracts verified bundled assets into
`$XDG_CACHE_HOME/gh-mirror/models/` (default `~/.cache/gh-mirror/models/`).
That cache directory must be writable on first use. `gh-mirror model` reports the
pinned model, revision, dimensionality and compatibility fingerprint.

Build vectors for an existing mirror without requesting GitHub data:

```sh
gh-mirror index
gh-mirror index --workers 8
gh-mirror search 'file transfers get stuck indefinitely' --engine semantic
gh-mirror search 'Acme attachments fail after reconnecting' --engine hybrid --label 'client:Acme'
gh-mirror candidates --repo owner/repository --number 123 --engine hybrid
```

`sync` now incrementally indexes vectors after committing collection and before
publishing its snapshot. `--index-workers` controls CPU concurrency separately from
GitHub request workers; when omitted, it inherits the effective `--workers` or
configured `workers` value. Standalone `index` also inherits configuration unless
its `--workers` flag overrides it. The built-in default remains four.
For example, `sync --workers 8` uses eight for each phase, while
`sync --workers 8 --index-workers 4` limits CPU indexing to four.
Use `sync --index=false` to skip vector indexing. Queries use `--engine lexical`
by default.

Indexing shows document progress, throughput, an ETA and active workers on stderr.
The sync display labels GitHub and CPU workers separately. A bounded document queue
keeps workers running across database pages, so one long document does not hold up
the next page of work.
Completed passages are committed as they finish. Interrupt with Ctrl-C and rerun
`index` to resume. Unchanged documents are skipped; changed documents reuse identical
passages and encode their new passages. Metadata-only changes do not re-encode text.
Inference runs in parallel while checkpoint writes queue for SQLite's single writer.
Readers remain concurrent, and each checkpoint retains FULL synchronous durability.
Deleted tickets/comments remove their vectors. Model or chunking changes rebuild
only the derived local vector index, without refetching GitHub data.
If collection has finished and indexing is interrupted, `index --workers 8` resumes
the remaining local work without repeating collection. Run `snapshot` afterward
to publish the completed index, or rerun `sync` to finish the normal workflow.

Titles, bodies, labels, discussion comments and inline review comments are indexed
as separate passages. Long text is split at the tokenizer's 256-token budget with
small overlaps, preserving its tail. Semantic evidence includes passage similarity
and byte offsets into the indicated field. Seed-based semantic candidates reuse
stored vectors for the entire seed title/body and selected labels/comments, avoiding
another inference pass. Recent seed comments remain opt-in and bounded.

`semantic` uses exact cosine similarity, with higher scores ranking first. `hybrid`
combines independent literal and semantic rankings using reciprocal rank fusion
(`1/(60+rank)` from each ranking). Results report the algorithm, `semantic_score`
and, where present, `lexical_score`. These scores are not duplicate probabilities.
Semantic retrieval ranks every eligible ticket with a selected indexed passage;
low-ranked results can be unrelated. Inspect evidence and bound the returned page.

Repository, label, state, project, author, date and other ticket filters work with
all three engines. `--in`, exclusions, counts, facets, views and pagination also
work with semantic/hybrid search. Literal `--match all`, `--match phrase` and
`--prefix` require the lexical engine. Natural-language queries are limited to
16,384 bytes and 16 passages; hybrid queries allow at most 512 literal terms.
An incomplete or incompatible index produces an explicit error with instructions
to run `index`; REST returns 503 with code `semantic_index_unavailable`.
`status.semantic` reports coverage, pending documents and model compatibility.
Semantic cursors also bind the vector generation and model fingerprint.

Snapshots include completed vectors in the same SQLite file. CI consumers acquire
that file and search offline with the same binary. They do not need a separate model
service or rebuild a complete compatible index. Exports omit incomplete/obsolete
passages while the collector retains its private resumable work. Existing schema-1
snapshots remain readable for lexical queries; `index` performs a local schema-2
migration. Older gh-mirror binaries cannot open schema-2 snapshots.

MiniLM is English-oriented. Hybrid search is useful for retaining exact client labels
and technical terms. The [model research](docs/semantic-model-research.md) compares
compact and larger alternatives, licenses and measurements on the target hardware.

## Query locally

Search issue titles, bodies, attached label names, and comments:

```sh
gh-mirror search 'socket timeout'
gh-mirror search 'socket timeout' --repo owner/repository --state closed --label bug --limit 20
gh-mirror search 'socket timeout' --type Bug
gh-mirror search 'Acme' --repo owner/repository
gh-mirror search 'timeout' --project owner/1 --kind issue
```

Queries use readable terminal output or piped JSON; `--format` overrides it.
The default `--match any` joins literal words with
OR. `--match all` requires every word somewhere in the selected ticket: a label and
a separate comment can satisfy it together. `--match phrase` requires consecutive
words in one source. Punctuation is tokenized; operators in the query are literal
words. Lexical queries exceeding 32 terms or 16,384 bytes fail explicitly.

```sh
gh-mirror search 'Acme timeout' --match all --count --facets
gh-mirror search 'socket timeout' --match phrase --in body --in comments
gh-mirror search 'auth' --prefix --in title --exclude-word deprecated
gh-mirror search 'timeout' --repositories owner/support --repositories owner/code --limit 20
```

`--prefix` matches word prefixes; in phrase mode it applies only to the final word.
`--in` selects title, body, labels, discussion comments (`comments`) or inline
review comments (`reviews`). Repeat it to select several sources; omitting it
searches all. Excluded words apply across the selected sources of the whole ticket.

Search returns compact summaries by default. Each match contains the legacy title,
URL, source, snippet and BM25 score, plus labels, assignees, issue type, milestone,
author, project identities and timestamps under `summary`. `evidence` contains up
to three matching sources with the field, excerpt, URL and comment identity/time.
Inline review evidence also includes stored file/line context and at most 1,024
characters of diff context. `--evidence-limit` accepts 1–10. Use `--view full` to add
raw ticket records, fields and memberships; comments are retrieved separately.

Search and listing return `has_more`, `next_cursor`, collection `status` and
structured `warnings`. Follow the cursor using the same query options; page size
can change. A changed generation requires restarting or querying an immutable
snapshot. `--count` adds `total` over all matching tickets before pagination.
`--facets` adds label/type/project counts over that same set, capped at 100 values
per facet with `truncated` indicating omitted values. These aggregate operations
are opt-in. Warnings identify uncollected repositories, disabled requested
resources and stale enrichment; `--max-enrichment-age 2h` changes the default
24-hour warning threshold for requested fields/projects.

`score` uses SQLite FTS5's [BM25 ranking](https://www.sqlite.org/fts5.html#the_bm25_function).
Lower, more negative scores rank first. Titles and label names have a weight of five and
body/comment text a weight of one. Compare scores within the same query; they are
text relevance values, not percentages or duplicate probabilities.

A client name that appears only in an attached label is searchable. `--label`
still applies an exact label filter. Label additions, renames, and removals update
the index with the issue. The next sync upgrades existing mirrors from stored
payloads without extra GitHub requests; publish or acquire a new snapshot to use
that index in a consumer. Unused label names are available through the catalog.

List tickets without requiring search words:

```sh
gh-mirror list --repo owner/repository --label 'client:Acme' --limit 100
gh-mirror list --kind pull_request --project owner/1 --limit 100
# Continue using next_cursor from the previous JSON, with the same filters:
gh-mirror list --repo owner/repository --label 'client:Acme' --limit 100 --cursor "$next_cursor"
```

Listing preserves full raw records by default; `--view summary` omits them and
returns common metadata. Default ordering is repository and ticket number.
Search defaults to relevance. Both support `--sort number`, `--sort updated` and
`--sort created`, with `--order asc` or `desc`; timestamps default to descending,
number/relevance to ascending. Ties use repository/number for stable continuation.
Project filters include archived memberships and use `owner/project-number`.

Search and listing share these filters:

| CLI flag | REST/MCP field | Meaning |
| --- | --- | --- |
| `--repo` | `repo` | One repository |
| `--repositories` | `repositories` | Any of these repositories; repeat |
| `--state`, `--kind`, `--type` | `state`, `kind`, `type` | Ticket state, issue/PR kind, exact native type |
| `--label`, `--labels-all` | `label`, `labels_all` | Require every exact label; repeat `labels-all` |
| `--labels-any` | `labels_any` | Require any listed exact label; repeat |
| `--exclude-label` | `exclude_labels` | Exclude each exact label; repeat |
| `--author` | `author` | Ticket author login |
| `--assignee` | `assignees` | Any selected assignee login; repeat |
| `--milestone` | `milestone` | Exact milestone title or number |
| `--created-after`, `--created-before` | `created_after`, `created_before` | Inclusive RFC3339 creation bounds |
| `--updated-after`, `--updated-before` | `updated_after`, `updated_before` | Inclusive RFC3339 update bounds |
| `--project` | `project` | Project membership owner/number |
| `--field 'Priority=High'` | REST: `field=Priority=High`; MCP: `field_values` | Require every collected field predicate; repeat |

MCP array fields accept JSON arrays. REST array fields use repeated parameters;
repeated `repo` parameters also select multiple repositories. MCP `field_values`
is an array of `{"name":"Priority","value":"High"}`. Native fields match text,
date and number values or selection option names. Singular and compound predicates
combine with AND; assignees and `labels_any` combine with OR within their lists.
Project membership filters do not change text ranking.

Read a complete issue with its comments and collected metadata, or retrieve
possible duplicates:

```sh
gh-mirror get --repo owner/repository --number 123
gh-mirror candidates --repo owner/repository --number 123 --limit 30
```

Candidates select up to 24 distinctive seed terms using local ticket frequency.
Titles receive more weight than descriptions, repeated boilerplate is de-duplicated,
and technical acronyms/error identifiers such as CSS, API and 403 are retained.
Attached seed labels contribute by default; use `--include-labels=false` to exclude
them. Recent seed comments are opt-in and bounded by `--comment-limit` (default 20,
maximum 100). Candidates include closed history and exclude the seed.

```sh
gh-mirror candidates --repo owner/support --number 123 --include-comments --count
gh-mirror candidates --repo owner/support --number 123 --repositories owner/support --repositories owner/code
gh-mirror get --repo owner/repository --number 123 --view summary
gh-mirror get --repo owner/repository --number 123 --comments none
gh-mirror get --repo owner/repository --number 123 --comments page --limit 20
gh-mirror comments --repo owner/repository --number 123 --kind review --limit 20
gh-mirror batch owner/repository#123 owner/repository#456
```

`get` retains the legacy full ticket and all comments by default. Summary view
omits comments by default; explicit `--comments none`, `page` or `all` controls
inclusion. Paged comments appear in `comment_page`. The `comments` command supports
`--kind all|discussion|review`, creation order and generation-bound cursors.
Summary comment previews stop at 1,024 Unicode characters and report `truncated`;
`--view full` preserves complete text and raw JSON. Comment IDs are strings, keeping
large numeric IDs intact and distinguishing discussion/review kinds. `batch` reads
up to 100 identities from one generation without comments, defaults to summaries,
and returns explicit `missing` identities. Repeat identities are de-duplicated.

Inspect labels, milestones, native issue types/fields, or project data:

```sh
gh-mirror catalog --kind labels --scope owner/repository
gh-mirror catalog --kind issue_fields --scope owner
gh-mirror catalog --kind projects --scope owner
# Enable bounded catalog pages; continue with --cursor from next_cursor:
gh-mirror catalog --kind labels --scope owner/repository --limit 30
```

| Catalog kinds | Scope |
| --- | --- |
| `labels`, `milestones` | `owner/repository` |
| `issue_types`, `issue_fields`, `projects` | `owner` |

Ticket results include memberships under `extra.projectItems`, with the project
ID, number, title, URL, and membership's archived state. Membership pagination is
included in the normal batched ticket metadata collection. Project coverage in
`status` is `memberships` when enabled and `disabled` when excluded.

Mirrors created by older versions retain existing membership metadata and remove
full project detail catalogs on the next successful sync, without forcing an extra
enrichment refresh. Compatible saved responses from an interrupted sync are reused
automatically; do not use `--restart` to upgrade.

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
curl 'http://127.0.0.1:8787/v1/search?q=file+transfers+get+stuck&engine=hybrid&limit=10'
curl 'http://127.0.0.1:8787/v1/issues/owner/repository/123'
```

| Endpoint | Purpose |
| --- | --- |
| `/health` | Health check |
| `/v1/status` | Generation, scope, coverage, and freshness |
| `GET /v1/search?q=...` | Search modes, source selection, shared filters, sorting, evidence, counts/facets and cursor |
| `GET /v1/issues` | Paginated listing with shared filters, projection and ordering |
| `GET /v1/issues/{owner}/{repo}/{number}` | Ticket; `view`, `comments`, `limit`, `cursor`, `comment_kind` control size |
| `GET /v1/issues/{owner}/{repo}/{number}/comments` | Bounded comments; `kind`, `view`, `order`, `limit`, `cursor` |
| `POST /v1/issues/batch` | Up to 100 ticket identities; JSON `tickets` and optional `view` |
| `GET /v1/catalog?kind=...&scope=...` | Add `limit`/`cursor` for bounded pages; legacy calls return the full catalog |
| `/v1/projects?owner=...&number=...` | Project catalog record and collection status |
| `/v1/candidates?repo=...&number=...&limit=...` | Possible duplicate issues |
| `/snapshots/latest` | Latest snapshot manifest |
| `/snapshots/{filename}` | Immutable snapshot file |

REST accepts `engine=lexical|semantic|hybrid` for search and candidates. It also
uses `match`, `prefix`, repeated `in` and `exclude_words` parameters for search.
`count` and `facets` are booleans. For example:

```sh
curl 'http://127.0.0.1:8787/v1/search?q=Acme+timeout&match=all&count=true&facets=true&limit=20'
curl 'http://127.0.0.1:8787/v1/issues/owner/repository/123?view=summary'
curl -X POST 'http://127.0.0.1:8787/v1/issues/batch' -H 'Content-Type: application/json' \
  --data '{"tickets":[{"repo":"owner/repository","number":123}],"view":"summary"}'
```

Errors contain `error` and a machine-readable `code`: invalid input is HTTP 400
(`invalid_query`), missing resources 404 (`not_found`), stale generation cursors 409
(`stale_cursor`), unavailable semantic indexes 503 (`semantic_index_unavailable`)
and internal failures 500 (`internal_error`). Lexical query upgrades work against
existing snapshots. Semantic engines require a compatible local index; building
it migrates the local schema without requesting GitHub data.

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

The ten read-only tools are `list_issues`, `search_issues`, `get_issue`, `get_issues`,
`list_comments`, `get_catalog`, `list_catalog`, `get_project`,
`find_duplicate_candidates`, and `get_sync_status`. Each advertises the schema of
its structured output envelope; preserved GitHub JSON remains unconstrained.
Use `list_catalog` for bounded pages and `get_catalog` for a legacy complete catalog.
`search_issues` and `find_duplicate_candidates` accept an `engine` argument with
the same three choices as REST and the CLI.
A compact workflow is search, inspect evidence, batch-fetch selected summaries,
then fetch only the required ticket details or comment pages.

## Evaluate retrieval

Supply local relevance judgments against an existing mirror or immutable snapshot:

```json
{
  "cases": [
    {
      "name": "Known socket timeout tickets",
      "search": {"query": "socket timeout", "match": "phrase"},
      "relevant": [{"repo": "owner/repository", "number": 123}]
    },
    {
      "name": "Known duplicate pair",
      "candidates": {"repo": "owner/repository", "number": 123},
      "relevant": [{"repo": "owner/repository", "number": 456}]
    }
  ]
}
```

```sh
gh-mirror --db ./mirror.sqlite evaluate --cases ./judgments.json
gh-mirror --db ./mirror.sqlite evaluate --cases ./judgments.json --format text
```

Each case requires exactly one search/candidate request and judged relevant ticket
identities. Evaluation retrieves the top 20, reports Precision@10 (relevant hits
divided by 10) and Recall@20 (retrieved relevant tickets divided by all judged
relevant tickets), separate search/candidate averages and observed p50/p95 latency.
Sparse judgments cap attainable Precision@10; judge all relevant results rather
than treating unjudged tickets as evidence of poor ranking. Evaluation fails if the
mirror generation changes and returns metrics without ticket content. It performs
no collection or remote model calls.

Versioned synthetic retrieval fixtures and a local benchmark live under
`internal/store`. They cover phrase search, labels plus comments, short technical
terms, same/cross-repository duplicate candidates and distractors. Run them with:

```sh
go test ./internal/store -run TestRetrievalEvaluation -v
go test ./internal/store -run '^$' -bench BenchmarkLocalQuery -benchtime=3x
go test ./internal/github -run '^$' -bench BenchmarkParallelPagination -benchtime=1x
```

Synthetic scores establish regression behavior, not real-world duplicate quality.
The [measured benchmark and historical duplicate evaluation](docs/benchmark-2026-10-10.md)
compares the three engines, worker scaling, latency and the limits of sparse judgments.
SQLite remains the only required retrieval engine. Use representative judgments
from your repositories to compare lexical, semantic and hybrid retrieval or decide whether reranking
justifies additional models, storage and deployment dependencies.

## Share a snapshot

Use snapshots to give another machine, CI job, or batch process a private copy of
one complete generation. `sync` publishes a snapshot by default; publish the last
successful collection separately with:

```sh
gh-mirror snapshot
```

The publisher exports a standalone SQLite file including its search index, removes
collector response caches and credential identity, compacts it to remove deleted
payload bytes, assigns
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
verifies SHA-256, schema and collection versions, and embedded metadata, then
atomically installs the private file. Legacy manifests without collection version
are interpreted as version 1. To require fresh fields and project memberships, add
`--max-enrichment-age 2h`; this is independent of `--max-age` for issue/comment
updates. The enrichment check is disabled by default.
For multiple repositories, repeat `--repo` or use a comma-separated list. Failed
acquisition preserves the previous destination. The destination must have no live
WAL sidecars; the current download limit is 2 GiB.

Keep the database pinned for the whole batch and retain the manifest to record its
generation and checksum. Snapshot files remain until you remove them. Serialize
publication and retain generations long enough for in-flight acquisitions to finish.

## Collection scope and efficiency

The selected scope includes accessible open and closed issues, PR ticket records,
discussion comments and optional inline review comments. Original REST JSON preserves bodies,
authors, labels, multiple assignees, state reasons, native types, and other returned
fields. Catalogs include unused labels, native issue fields, and available projects.
Ticket metadata includes project memberships with active and archived states.

PR approval reviews, Discussions, attachment binaries, and historical
deleted text are outside this version. Full project items, draft cards, and project
fields/values are also excluded. The mirror reflects the credential's visible
scope and the last successful collection, rather than live GitHub state.

Bootstrap fetches issues and repository-wide comments in pages of 100. Native
fields and relationships use GraphQL batches of 50 tickets across repositories.
Owner catalogs and projects are collected once per owner. Project memberships are
returned in the ticket batches, with additional requests only for nested pagination.

When GitHub supplies numbered next/last page links, even a single long listing can
use all configured `--workers`. Pages share the global request limit and retry
pauses; completed pages are checkpointed immediately and assembled in page order.
This fetches the same pages as serial collection, without speculative requests.
Cursor-dependent listings and listings without a known last page remain sequential.
Repositories are processed in order. Existing saved pages remain reusable after
upgrading: interrupt with Ctrl-C and rerun `sync`, without `--restart`.

Between scheduled enrichment and full reconciliation, an unchanged repository
requires two requests when discussion comments are enabled: one issue delta listing
and one comment delta listing. Enabling inline review comments adds one repository
review-comment listing; disabling all comments leaves one issue delta listing.
Polling comments separately catches edits to comments on older issues. The overlap
window avoids gaps; unchanged overlapping issues do not trigger extra hydration.
Changed or new issues are hydrated in batches. Reusable listings use conditional
requests; a 304 still counts as a request.
If a comment references an issue missing from the issue inventory, sync fetches that
parent once and includes it in the mirror and metadata batches. Existing parents
require no additional requests during incremental updates. Recovery fetches are
saved for resume along with listing pages.

Each sync reports its actual HTTP attempt count. Pagination, GraphQL, and retries
share `max_requests` across workers; exhausting the budget or encountering a long
rate-limit wait stops collection and retains completed fetches for the next run.
The budget applies separately to each invocation. Longer refresh intervals reduce requests
at the cost of older field, project, or deletion information.

## Development and diagnostics

GitHub Actions runs formatting, vetting, build, race tests and linting on pushes
and pull requests. Run `make release` to package Linux/macOS amd64/arm64 archives.
Pushing a `vMAJOR.MINOR.PATCH` tag runs checks and publishes the archives and SHA-256
checksums; the tag must match the version computed by monova.

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
[Projects](https://docs.github.com/en/rest/projects/projects). The REST version is
`2026-03-10`. Feature availability on GitHub Enterprise Server may differ; unsupported
enabled features fail rather than being reported as empty complete catalogs.
Request coordination follows GitHub's
[rate-limit guidance](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api).
GitHub [recommends serial requests](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api)
to reduce secondary rate limits; `--workers 1` selects that mode.
