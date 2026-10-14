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
piped. Search and related-ticket results show ranked tickets, labels and evidence;
`evaluate` shows a metrics table. Select a format explicitly with `--format`:

```sh
gh-mirror search 'socket timeout' --format text
gh-mirror related owner/repository#123 --format text
gh-mirror status --format json
gh-mirror search 'socket timeout' | jq '.matches'
```

`--format json` preserves the complete structured output. Text results use compact
previews; REST and MCP always retain their structured contracts.

Shell completion suggests configured repositories, label names from the local
database, catalog kinds and fixed flag values. Install it for your shell:

```sh
gh-mirror completion bash > ~/.local/share/bash-completion/completions/gh-mirror
gh-mirror completion zsh > "${fpath[1]}/_gh-mirror"
gh-mirror completion fish > ~/.config/fish/completions/gh-mirror.fish
```

## Configure

Copy [examples/config.toml](examples/config.toml) into
`~/.config/gh-mirror/config.toml` and replace `owner/repository`:

```sh
mkdir -p ~/.config/gh-mirror
cp examples/config.toml ~/.config/gh-mirror/config.toml
```

A minimal configuration can contain one repository table:

```toml
workers = 8

[repos."owner/repository"]
preset = "issues"
```

Omitted settings retain their defaults. TOML is preferred when both configuration
files exist; an existing `config.json` remains supported with its original scope
semantics. Convert it without overwriting either file:

```sh
gh-mirror config migrate
gh-mirror config show
gh-mirror config show --effective
```

`config show` prints compact TOML, including when piped. `--effective` expands all
defaults and repository switches. Explicit `--format json` prints resolved JSON.

Commands that read one ticket accept `#123` or `123` when a default repository is
known. Set it with `default_repo = "owner/repository"`; with exactly one configured
repository, that repository is the default.

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
403/404 or GraphQL errors fail collection. Disable unavailable resources in the
repository table; status records the resolved scope.
Personal repositories have no organization issue fields. User-owned Projects REST
endpoints require a compatible classic PAT; GitHub's current documentation excludes
fine-grained and installation tokens for those endpoints.
`projects` collects available project catalogs and ticket memberships. It does not
fetch complete project items, draft cards, or project field definitions/values.

## Select resources per repository

TOML repository tables replace the separate repository list. Choose a preset,
then override any switch with `true` or `false`:

```toml
[repos."owner/repository"]
preset = "all"
pull_request_review_comments = false

[repos."owner/code"]
preset = "pull_requests"
```

`all` enables all eleven switches. `issues` enables issues, discussion comments
and the six metadata/relationship/catalog switches. `pull_requests` enables PR
records, discussion comments and inline review comments, with other switches off.
Overrides inherit the preset. A table without a preset inherits the legacy global
defaults, including disabled inline review comments.

Legacy `repositories` and `repository_options` remain accepted in JSON or TOML;
omitted switches in an explicit legacy options entry remain false. Do not mix
legacy repository keys with `repos`. Unknown settings and presets are rejected.
Migration selects a compact preset with explicit overrides that preserve every
resolved switch.

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

## Query locally

Queries read the local database and need no GitHub credential. Use
`gh-mirror --db /path/to/mirror.sqlite ...` to query another database, including an
acquired snapshot.

| Command | Purpose |
| --- | --- |
| `search WORDS` | Rank tickets by words or meaning, with source evidence |
| `list` | List tickets by filters without search words |
| `show REF...` | Read up to 100 tickets without comments |
| `comments REF` | Page through discussion and review comments |
| `context REF` | Read bounded evidence for evaluation, classification, duplicates or proofreading |
| `related REF` | Find related tickets and duplicate candidates for a ticket |
| `catalog KIND [SCOPE]` | Read labels, milestones, issue types, issue fields or projects |

`REF` is `owner/repo#123`, a GitHub issue or pull request URL, or `#123`/`123` with
a default repository. The `--repo` and `--number` flags remain accepted.

```sh
gh-mirror search 'socket timeout' --repo owner/repository --state closed --labels-all bug
gh-mirror show owner/repository#123 https://github.com/owner/repository/pull/456
gh-mirror comments owner/repository#123 --kind review --limit 20
gh-mirror context '#123' --query 'session continuation'
gh-mirror related 123 --include-comments --labels-any 'client:Acme'
gh-mirror catalog labels
gh-mirror catalog projects owner
```

Every query command returns summaries by default; `--view full` adds raw GitHub
records. The text view of `show` lists the title, state, kind, labels, assignees,
milestone, type, URL, dates, comment count and the start of the body. `--view full`
prints the whole body, and `--view full --format json` returns the raw records.
`show` never includes comments; read them with `comments`.

The commands `get`, `batch` and `candidates` from earlier releases still work.
They are hidden from help and print a notice naming `show`, `comments` or `related`.

### Search

Search defaults to hybrid retrieval across all collected source fields and both open
and closed tickets. In lexical retrieval, `--match any` joins literal words with
OR. `--match all` requires every word somewhere in the selected ticket: a label and
a separate comment can satisfy it together. `--match phrase` requires consecutive
words in one source. Punctuation is tokenized; operators in the query are literal
words. Lexical queries exceeding 32 terms or 16,384 bytes fail explicitly.

```sh
gh-mirror search 'Acme timeout' --match all --count --facets
gh-mirror search 'socket timeout' --match phrase --in body --in comments
gh-mirror search 'auth' --prefix --in title --exclude-word deprecated
gh-mirror search 'timeout' --repositories owner/support --repositories owner/code --limit 20
gh-mirror search 'timeout' --project owner/1 --kind issue --type Bug
```

`--prefix` matches word prefixes; in phrase mode it applies only to the final word.
`--in` selects title, body, labels, discussion comments (`comments`) or inline
review comments (`reviews`). Repeat it to select several sources; omitting it
searches all. Excluded words apply across the selected sources of the whole ticket.

Each match contains the title, URL, source, snippet and the engine's score, plus
labels, assignees, issue type, milestone, author, project identities and timestamps
under `summary`. `evidence` holds up to three matching sources (`--evidence-limit`
accepts 1–10) with the field, excerpt, URL and comment identity. Inline review
evidence includes file/line context and at most 1,024 characters of diff.

Search, listing and related tickets return `has_more`, `next_cursor`, collection
`status` and structured `warnings`. Follow the cursor with the same options; page
size can change. A changed generation requires restarting or querying an immutable
snapshot. `--count` adds `total` before pagination. Lexical totals count literal
matches; semantic and hybrid totals count the retrieved set, so they stay the same
on every page. `--facets` adds label, type and
project counts over the same set, capped at 100 values per facet. Warnings identify
uncollected repositories, disabled resources and stale enrichment;
`--max-enrichment-age 2h` changes the default 24-hour warning threshold.

With `--engine lexical`, `score` is the negated SQLite FTS5 [BM25 ranking](https://www.sqlite.org/fts5.html#the_bm25_function),
as is hybrid `lexical_score`. Higher scores rank first for every engine. Lexical
cursors from releases that reported raw BM25 return a stale-cursor conflict.
Titles and label names have a weight of five and body/comment text a weight of one. Scores rank results within one query; they
are not percentages or duplicate probabilities. A client name that appears only in
an attached label is searchable.

### Filters and ordering

`list` orders by repository and ticket number; search orders by relevance. Both
accept `--sort number|updated|created` and `--order asc|desc`; timestamps and relevance
default to descending (best first), number to ascending. Ties use repository and number for stable continuation. `search`,
`list` and `related` share these filters:

| CLI flag | REST/MCP field | Meaning |
| --- | --- | --- |
| `--repo` | `repo` | One repository (search and list) |
| `--repositories` | `repositories` | Any of these repositories; repeat |
| `--state`, `--kind`, `--type` | `state`, `kind`, `type` | Ticket state, issue/PR kind, exact native type (search and list) |
| `--labels-all` | `labels_all` | Require every exact label; repeat |
| `--labels-any` | `labels_any` | Require any listed exact label; repeat |
| `--exclude-label` | `exclude_labels` | Exclude each exact label; repeat |
| `--author` | `author` | Ticket author login |
| `--assignee` | `assignees` | Any selected assignee login; repeat |
| `--milestone` | `milestone` | Exact milestone title or number |
| `--created-after`, `--created-before` | `created_after`, `created_before` | Inclusive creation bounds |
| `--updated-after`, `--updated-before` | `updated_after`, `updated_before` | Inclusive update bounds |
| `--project` | `project` | Project membership owner/number, including archived memberships (search and list) |
| `--field 'Priority=High'` | REST: `field=Priority=High`; MCP: `field_values` | Require every collected field predicate; repeat |

Date bounds accept RFC3339 timestamps, `YYYY-MM-DD` dates and relative ages such
as `12h`, `7d` or `2w`. A date starts at UTC midnight for `*-after` and covers the
whole UTC day for `*-before`. A relative age means that long before the query runs.

MCP array fields accept JSON arrays. REST array fields use repeated parameters;
repeated `repo` parameters also select multiple repositories. MCP `field_values`
is an array of `{"name":"Priority","value":"High"}`. Native fields match text,
date and number values or selection option names. Predicates combine with AND;
assignees and `labels_any` combine with OR within their lists. The hidden `--label`
flag is an alias for `--labels-all`.

```sh
gh-mirror list --repo owner/repository --labels-all 'client:Acme' --limit 100
# Continue using next_cursor from the previous JSON, with the same filters:
gh-mirror list --repo owner/repository --labels-all 'client:Acme' --limit 100 --cursor "$next_cursor"
```

### Tickets and comments

`show` reads up to 100 tickets from one generation, de-duplicates them and reports
missing ones under `missing`. `comments` pages through `--kind all|discussion|review`
comments in creation order. Summary previews stop at 1,024 Unicode characters and
report `truncated`; `--view full` preserves complete text and raw JSON.
`excluded_by_policy` and its warning count only comments of the selected kind.
Disabled review collection is reported only for pull requests or `--kind review`. Comment IDs
are strings, keeping large numeric IDs intact.

### Related tickets

`related` selects up to 24 distinctive seed terms using local ticket frequency.
Titles receive more weight than descriptions, repeated boilerplate is de-duplicated,
and technical acronyms and error identifiers such as CSS, API and 403 are retained.
Attached seed labels contribute by default; `--include-labels=false` excludes them.
Recent seed comments are opt-in with `--include-comments` and bounded by
`--comment-limit` (default 20, maximum 100). Results include closed history,
exclude the seed and default to the seed repository; `--repositories` widens it.

```sh
gh-mirror related owner/support#123 --include-comments --count
gh-mirror related owner/support#123 --repositories owner/support --repositories owner/code
```

### Ticket context for agents

`context` reads bounded evidence for evaluating one ticket:

```sh
gh-mirror context owner/repository#123 --query 'session continuation' --format text
gh-mirror context owner/repository#123 --purpose proofread --max-characters 20000
```

The same view is available at `GET /v1/issues/owner/repository/123/context` and
through the MCP `get_issue_context` tool. It includes original description text,
section byte offsets, absent or empty sections, structured label attributes,
closure and merge metadata, selected relevant and recent comments, and bounded
one-hop related tickets. References distinguish native relationships, mentions,
explicit duplicate references and fix references. Shorthand `owner/repo#N` and `#N`
must not follow a word character, slash, colon or dot, so version strings and URL
fragments are ignored. Links count only as GitHub issue or pull request URLs.
These are source observations;
an agent must evaluate whether two tickets describe the same problem.

`--comment-limit`, `--related-limit` and `--max-characters` bound the response.
Coverage reports omitted comments, relationship expansion and truncated text.
`comments_total` counts comments eligible for selection; policy exclusions appear
only in `comments_excluded_by_policy` unless `--include-excluded` is set.
Proofreading gives the original description priority; other purposes reserve
space for discussion. Full source records remain available through `show --view full`
and `comments --view full`. Section extraction preserves Markdown, code and Unicode.

Labels such as `client:Acme`, `feature:continuation` and `comp:browser` expose
roles and values without discarding their original names. Multiple labels in a
role remain separate. Configure other conventions per repository:

```json
{
  "label_rules": {
    "owner/repository": [
      {"prefix": "customer:", "role": "client"},
      {"label": "account:example", "role": "client", "value": "Example",
       "aliases": ["Example Ltd"], "description": "Affected customer"}
    ]
  }
}
```

Run `gh-mirror index` to save rules in the snapshot locally. `sync` also saves
rules when indexing is enabled. Exact rules override prefix rules; the longest
matching prefix wins. Attached label catalog definitions accompany context when
available. Changing rules does not fetch GitHub or change the embedding model.

### Catalogs

| Catalog kinds | Default SCOPE |
| --- | --- |
| `labels`, `milestones` | Default repository |
| `issue_types`, `issue_fields`, `projects` | Owner of the default repository |

Other kinds are rejected with an error that lists the supported kinds.

`catalog` returns the whole catalog. `--limit N` or `--cursor` switches to bounded
pages. Ticket results include project memberships under `extra.projectItems`, with
the project ID, number, title, URL and archived state. Project coverage in `status`
is `memberships` when enabled and `disabled` when excluded.

### Retrieve context for ticket evaluation

Agents can use the mirror to investigate behavior, find earlier decisions and
workarounds, connect a report to previous fixes, and assess its scope. Duplicate
discovery is one use of the same retrieval and reading tools.

Start with a focused question and any useful exact terms, such as a client label,
error message or component name. Search includes titles, bodies, attached labels,
discussion comments and inline reviews by default. Retain closed history when
looking for resolutions. Repository, project and label filters narrow the context;
use multiple repositories when support tickets and code history live separately.

```sh
gh-mirror search 'workaround for downloads after reconnecting' --limit 10
gh-mirror search 'session token retry fix' --in reviews --limit 10
gh-mirror search 'file transfer failure' --labels-all 'client:Acme' --evidence-limit 5
gh-mirror search 'ECONNRESET' --engine lexical
gh-mirror show owner/support#123 owner/code#456
gh-mirror comments owner/support#123 --view full --limit 30
```

For MCP, use `search_issues` for questions and `find_duplicate_candidates` for
history related to a known seed ticket. Inspect summaries and source-linked
`evidence`, then use `get_issues` to batch-read selected tickets, `get_issue` for
one ticket, and `list_comments` for bounded discussion
or review pages. Follow comment cursors if more context is needed. Cite the source
URLs supporting an assessment and check `status` and `warnings` for stale or disabled
resources. A search snippet is an excerpt; read the surrounding discussion before
interpreting a decision or fix.

Scores order potentially useful context. They do not establish relevance, correctness
or duplicate status, and semantic retrieval can return unrelated low-ranked tickets.
The initial duplicate-reference benchmark supports the hybrid default, but broader
ticket-evaluation quality needs reviewed queries about causes, workarounds, decisions
and fixes. The `evaluate` command accepts those relevance judgments too.

## Sync

The first sync creates the database and collects the configured repositories.
Later runs update the same database incrementally:

```sh
gh-mirror sync
gh-mirror status
```

`sync` runs once and exits. It publishes a portable snapshot after successful
collection. Use `sync --publish=false` if you only need the local database.
`status` reports the absolute database path (`db_path`), generation, repository
scope, coverage, counts, and collection and enrichment timestamps. It adds
`pending_sync` while an interrupted sync has unfinished work. A failed collection rolls back its data, indexes, and
checkpoints together, leaving the previous successful generation available.

`doctor` checks the configuration, the GitHub token variable, the database,
collection age (`--max-age`, default 24h), pending sync work, semantic index
coverage, the encoder and the saved PR comment policy. It prints one ✓, ! or ✗
line per check and exits with status 1 when a check fails.

```sh
gh-mirror doctor
gh-mirror doctor --max-age 2h --format json
```

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
Scheduled enrichment and reconciliation are measured from the current time, so a
sync that keeps failing still reaches them. Each failed attempt records its error
in the pending file.

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
*/15 * * * * /absolute/path/gh-mirror --config /absolute/path/config.toml sync --publish=false
```

Supply `GITHUB_TOKEN` to the scheduler and keep the database on persistent storage.
Run one collector at a time. `serve` and `mcp` read the database; they do not
schedule updates themselves.

Issue and comment changes are fetched on each sync. Catalogs, native fields,
relationships, and projects are refreshed every `enrichment_interval` (default
one hour). Full issue/comment inventories are reconciled every
`reconcile_interval` (default 24 hours) to remove deleted or transferred records.
These separate refreshes cover changes GitHub does not reliably expose through
issue timestamps. Before removing a ticket missing from a complete listing, sync
fetches it individually and keeps it if GitHub still returns it. A ticket deleted
between reconciliations no longer blocks enrichment; the next sync reconciles its
repository. Renamed or deleted labels cause the enrichment refresh to refetch the
tickets that carried them.

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

All tools are read-only. By default the server lists seven core tools; `mcp --tools all`
and `serve --mcp-tools all` add four more.

| Tool | Set | Purpose |
| --- | --- | --- |
| `search_issues` | core | Search with filters, evidence and cursors |
| `find_duplicate_candidates` | core | Related tickets for a seed ticket |
| `get_issue_context` | core | Bounded evidence for one ticket |
| `get_issues` | core | Up to 100 tickets without comments |
| `get_issue` | core | One ticket; summary without comments by default |
| `list_comments` | core | A bounded page of comments |
| `get_sync_status` | core | Full generation, scope, coverage and freshness |
| `list_issues` | all | Filtered listing without search words |
| `get_catalog` | all | A complete catalog |
| `list_catalog` | all | A bounded catalog page |
| `get_project` | all | A project catalog record |

Every input field has a description, and fixed choices such as `view`, `engine` and
`sort` list their allowed values. Tools do not advertise output schemas. Results
carry `status` with only `generation` and `collected_at`; `get_sync_status` returns
the full status. `get_issue` defaults to `view=summary` and `comments=none`; pass
`comments=page` for a bounded page or `comments=all` for every comment.
`search_issues` and `find_duplicate_candidates` accept `engine` with the same
choices and hybrid default as REST and the CLI. Read `query.engine`, `ranking` and
`warnings` to identify the effective engine, score direction and coverage.

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
| `GET /v1/issues/{owner}/{repo}/{number}/context` | Bounded ticket evidence; `purpose`, `query`, limits |
| `GET /v1/issues/{owner}/{repo}/{number}/related` | Related tickets with shared filters, sorting and cursor |
| `/v1/candidates?repo=...&number=...` | Deprecated alias of `/related` |
| `/snapshots/latest` | Latest snapshot manifest |
| `/snapshots/{filename}` | Immutable snapshot file |

REST accepts `engine=lexical|semantic|hybrid` for search and related tickets; omitting it
uses hybrid with a warned lexical fallback when vector coverage is unavailable. It also
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

## Snapshots

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

## Advanced

### Offline semantic and hybrid search

The executable includes MiniLM L6 weights, its WordPiece tokenizer and model
configuration (about 91 MB of model assets). Inference defaults to Go on the CPU.
The first use extracts verified bundled assets into
`$XDG_CACHE_HOME/gh-mirror/models/` (default `~/.cache/gh-mirror/models/`).
That cache directory must be writable on first use. `gh-mirror model` reports the
pinned model, revision, dimensionality and compatibility fingerprint.

#### Optional Vulkan embeddings

On a machine with Lemonade installed, its Vulkan runtime can accelerate indexing
and query embeddings. Start Lemonade for the installation step, then install its
backend if it is absent:

```sh
lemonade --no-discovery backends install llamacpp:vulkan
gh-mirror model --check --embedding-backend lemonade-vulkan --format text
gh-mirror index --workers 8 --embedding-backend lemonade-vulkan
gh-mirror search 'file transfers stop after reconnecting' --engine hybrid --embedding-backend lemonade-vulkan
```

gh-mirror starts a dedicated, authenticated localhost `llama-server` from
Lemonade's installed Vulkan backend. It uses a local FP32 GGUF export of the exact
bundled MiniLM tensors, passes the bundled tokenizer's token IDs directly, and
selects mean pooling and L2 normalization. Export needs about 87 MiB of additional
cache space and no Python conversion tools. The process runs with `--offline`,
does not use the Lemonade daemon during inference, and stops when the command
exits. On Linux it also stops if gh-mirror is killed. The local API key is passed
through a private temporary file rather than the command line. A standalone installation can continue using the bundled Go CPU encoder.

Persist the choice in your configuration:

```json
"embedding": {
  "backend": "lemonade-vulkan",
  "batch_size": 8,
  "device": "Vulkan0"
}
```

The default executable is under the user's Lemonade cache at
`lemonade/bin/llamacpp/vulkan/llama-server` (`.exe` on Windows). Set
`embedding.runtime` or `--embedding-runtime` to an absolute path if it is installed
elsewhere. `--embedding-device` selects a numbered Vulkan device;
`--embedding-batch-size` bounds batches between 1 and 32 passages (default 8).
Index workers
prepare documents and commit results; the GPU queue combines passages from those
workers, including batches within long documents. REST and MCP use the configured
encoder for semantic and hybrid queries.

Before accepting GPU vectors, gh-mirror verifies complete Vulkan layer offload and
compares reference vectors, tokenizer IDs, and a full 256-token passage. Each
comparison requires cosine similarity of at least 0.9999 and a maximum coordinate
difference of 0.001. The runtimes use different floating point implementations,
so their outputs need not be identical. Every response must retain model identity,
ordering, dimensionality, finite coordinates and unit norm.

Missing runtimes/devices, failed verification and inference errors trigger CPU
fallback with a visible reason. No alternate model is selected. Progress shows the
effective backend/device, actual batch size and vectors per second in addition to
document throughput. Switching compatible CPU/Vulkan backends preserves saved
vectors and checkpoints. A changed model fingerprint rebuilds vectors from stored
documents through `index`, without GitHub requests. Use `--embedding-backend cpu`
to override a configured GPU backend.
The [Vulkan benchmark](docs/vulkan-benchmark-2026-10-10.md) records model
compatibility, indexing throughput, query latency and interruption tests on
Ryzen AI / Radeon 890M hardware.

Build vectors for an existing mirror without requesting GitHub data:

```sh
gh-mirror index
gh-mirror index --workers 8
gh-mirror search 'file transfers get stuck indefinitely' --engine semantic
gh-mirror search 'Acme attachments fail after reconnecting' --engine hybrid --labels-all 'client:Acme'
gh-mirror related owner/repository#123 --engine hybrid
```

`sync` now incrementally indexes vectors after committing collection and before
publishing its snapshot. `--index-workers` controls CPU concurrency separately from
GitHub request workers; when omitted, it inherits the effective `--workers` or
configured `workers` value. Standalone `index` also inherits configuration unless
its `--workers` flag overrides it. The built-in default remains four.
For example, `sync --workers 8` uses eight for each phase, while
`sync --workers 8 --index-workers 4` limits CPU indexing to four.
Use `sync --index=false` to skip vector indexing. Search and related tickets default
to hybrid retrieval, combining exact words with similarity in meaning.
Use `--engine lexical` for literal retrieval or `--engine semantic` for similarity alone.
When no engine is supplied and the selected vector index is missing, incomplete
or incompatible, retrieval falls back to lexical with a `search_engine_fallback`
warning and instructions to run `index`. Results identify the actual engine and
its score meaning. Explicit `--engine hybrid` or `--engine semantic` require a
complete compatible index and return an error when it is unavailable.

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
combines lexical, semantic and whole-ticket evidence rankings using reciprocal rank
fusion: `1/(60+lexical rank) + 1/(60+semantic rank) + 3/(60+ticket rank)`.
Ticket evidence rewards distinctive query terms across selected fields and client
or partner label matches, including configured aliases. Entity matches are soft
boosts; explicit label filters remain hard constraints. Semantic ranking combines
the strongest passage with the best evidence per field, so repeated comments do
not multiply a ticket’s weight. Results report detected query entities, matched
terms/fields, component ranks, the algorithm, `semantic_score`
and, where present, `lexical_score`. These scores are neither relevance percentages
nor duplicate probabilities.
Semantic retrieval returns the 200 tickets with the most similar selected passages;
low-ranked results can be unrelated. Hybrid retrieval returns every literal or
ticket evidence match plus those 200 semantic results, so literal matches without
an indexed passage in the selected fields still appear. Inspect evidence and bound
the returned page.

Repository, label, state, project, author, date and other ticket filters work with
all three engines. `--in`, exclusions, counts, facets, views and pagination also
work with semantic/hybrid search. Literal `--match all`, `--match phrase` and
`--prefix` select lexical retrieval when no engine is supplied, with a
`search_engine_selected` explanation. They require lexical when an engine is explicit.
Natural-language queries are limited to
16,384 bytes and 16 passages; hybrid queries allow at most 512 literal terms.
For explicit semantic/hybrid requests, an incomplete or incompatible index produces
an error with instructions to run `index`; REST returns 503 with code
`semantic_index_unavailable`. Automatic fallback applies only to unavailable vector
coverage; input errors, inference failures and database errors are returned.
`status.semantic` reports coverage, pending documents and model compatibility.
Semantic cursors also bind the vector generation and model fingerprint.
They bind the actual query vector too: small numerical changes during CPU/GPU
fallback invalidate pagination with a stale-cursor response. A change between
lexical and hybrid retrieval also invalidates continuation. Restart the query
after that response or when continuing a semantic cursor from an older binary.

Snapshots include completed vectors in the same SQLite file. CI consumers acquire
that file and search offline with the same binary. They do not need a separate model
service or rebuild a complete compatible index. Exports omit incomplete/obsolete
passages while the collector retains its private resumable work. Existing schema-1
snapshots remain readable for lexical queries, and schema-2 snapshots remain
readable with their existing vectors. `index` performs a local schema-3 migration
for shared embedding storage. Older binaries cannot open schema-3 snapshots.

MiniLM is English-oriented. Hybrid search is useful for retaining exact client labels
and technical terms. The [model research](docs/semantic-model-research.md) compares
compact and larger alternatives, licenses and measurements on the target hardware.

### Filter AI review comments

PR descriptions remain searchable. To exclude known AI reviewer comments and
inline threads they start from retrieval:

```toml
[pr_comments]
exclude_known_ai = true
exclude_ai_threads = true
```

The bundled [account registry](internal/commentpolicy/registry.md) recognizes
Copilot, CodeRabbit, Gemini Code Assist and Claude using stored author identities,
without extra API requests or inference. Known final-line Claude Code attribution,
such as `Addressed by [Claude Code](https://claude.com/claude-code)`, also identifies
individual comments posted by human accounts. Quoted/code examples and mere mentions
are ignored. Unknown bots and unmarked human posts remain included. Filtering is disabled
by default for compatibility with existing configurations.

`exclude_ai_threads` excludes replies under an excluded inline review root. An AI
reply to a human-started thread excludes that reply while retaining human comments.
General PR discussion comments have no native threads and are filtered individually.
The same policy applies to lexical/hybrid/semantic evidence, duplicate seeds and
candidates, bounded comment pages and context. Issue comments remain unaffected.

Additional settings and per-repository overrides:

```toml
[pr_comments]
exclude_known_ai = true
exclude_ai_threads = true
exclude_all_bots = false
exclude_authors = ["custom-reviewer"]
include_authors = ["trusted-reviewer[bot]"]

[repos."owner/code".pr_comments]
exclude_known_ai = false
exclude_authors = []
```

Overrides inherit global settings. Supplied author lists replace inherited lists;
empty lists clear them. Logins match exactly, ignoring case. Include exceptions
win, including inside excluded threads. `exclude_all_bots` also excludes coverage,
security and dependency bots; it is independent of known-AI detection.

All raw comments remain in the mirror and snapshots. Filtering saves embedding
work and reduces retrieval noise, while bulk GitHub comment requests stay the same.
Apply changed settings locally with `index`; `sync` also applies the policy, including
with `--index=false`. Readers use the policy saved in their database or snapshot;
a configuration edit takes effect after `index` or `sync`.
Standalone indexing without configured repositories preserves the mirror's saved
policy. Configure repository tables when changing retrieval rules.

```sh
gh-mirror index
gh-mirror bots --format text
gh-mirror status --format text
gh-mirror comments owner/code#123 --include-excluded --format text
gh-mirror context owner/code#123 --include-excluded --format text
```

`bots` reports detected accounts, provider identities and applied exclusion counts.
It distinguishes account detection from per-comment declared attribution, including
human accounts that post both AI-attributed and ordinary comments. Custom workflows
can append `<!-- gh-mirror:ai-generated:claude -->` on its own final line to declare
AI-generated comments explicitly.
Coverage and warnings report omitted comments. REST and MCP comment/context reads
accept `include_excluded=true`; paged `get_issue` does too. `get_issue` with
`comments=all` retains every source comment. Policy changes invalidate retrieval cursors without
changing collection checkpoints, the model or tokenizer. Unchanged vectors and
indexing checkpoints remain reusable. Disabling filtering restores source documents
locally and embeds only restored passages; exports omit unused shared vectors.
Evaluation reports record the retrieval generation and reject concurrent policy,
vector or collection changes.

### Evaluate retrieval

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

### Derived indexing and shared embeddings

The local representation version is independent of GitHub collection and the
embedding model. `index` rebuilds derived passages after representation changes
without refetching tickets or discarding GitHub response checkpoints. Ticket body
passages omit empty template answers and known review checklists while retaining
exact original byte offsets. Raw bodies and the lexical source index are preserved.

Identical embedding inputs share one SQLite vector, keyed by the model fingerprint,
representation version and exact text. Concurrent indexing workers share in-flight
inference, including duplicates within batches. Saved vectors survive cancellation;
resuming reuses completed sources and shared inputs. New vectors store source
attribution separately from the shared value. Schema-2 vectors remain readable
and migrate locally; unchanged passage vectors are reused during the first rebuild.
Exports remove incomplete source passages and unreferenced shared vectors.

Classification context includes compact label names and descriptions, prioritizing
attached labels, and the configured role rules. Label definitions use at most a
third of the text budget, after ticket evidence and configured rules. The text view
shows these definitions, label interpretations, custom fields and related ticket
excerpts. Catalog entries, section lists and relationship lists have bounded counts;
coverage reports omissions with one warning for omitted labels. Use `catalog labels
owner/repository` to read complete GitHub label records and additional
labels before assigning a new classification.

Hybrid duplicate candidates retain weighted seed-vector and lexical rank fusion.
They use a different ranking policy from information searches, preserving recall
without promoting query-specific client matches. Whole-ticket match signals use
`ticket` for title/body document coverage; passages identify the exact field.

### Collection scope and efficiency

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
share `max_requests` across workers. Secondary rate limits wait at least one minute
and back off for up to five attempts; an exhausted quota waits until GitHub's reset.
Exhausting the budget or a rate-limit wait over 61 minutes stops collection and
retains completed fetches for the next run.
The budget applies separately to each invocation. Longer refresh intervals reduce requests
at the cost of older field, project, or deletion information.

### Development and diagnostics

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
request/count/status metadata to `gh-mirror-<uid>.log` in the temporary directory
(normally `/tmp`), truncated at startup. It refuses to follow symbolic or hard links.
Credentials and ticket bodies are excluded from diagnostic logs.

### API references

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
