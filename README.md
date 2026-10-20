# gh-mirror

gh-mirror copies GitHub tickets into a local SQLite database: issues, pull request
records, comments, labels, native issue fields and project memberships. You collect
once, then update incrementally. Queries run offline from that database.

One Go executable provides:

- **A CLI** for search, listing and reading tickets.
- **MCP tools** for AI agents, over stdio or HTTP.
- **A REST API** for scripts and services.
- **Portable snapshots** that other machines and CI jobs can query.

It needs no external database or search service. Only `sync` talks to GitHub, and it
never writes there.

## Quick start

```sh
mkdir -p ~/.config/gh-mirror
cat > ~/.config/gh-mirror/config.toml <<'EOF'
default_repo = "owner/repository"

[repos."owner/repository"]
preset = "issues"
EOF

export GITHUB_TOKEN=...          # read access to the repository
gh-mirror sync                   # first run collects everything; later runs update
gh-mirror search 'socket timeout'
gh-mirror show '#123'
gh-mirror related '#123'
gh-mirror doctor
```

## Contents

- [Install](#install)
- [Configure](#configure)
- [Query](#query)
- [Sync](#sync)
- [Snapshots](#snapshots)
- [MCP](#mcp)
- [REST API](#rest-api)
- [Semantic and hybrid search](#semantic-and-hybrid-search)
- [AI review comment filtering](#ai-review-comment-filtering)
- [Evaluate retrieval](#evaluate-retrieval)
- [Development](#development)

## Install

Install and update with [grm](https://github.com/jsnjack/grm):

```sh
grm install jsnjack/gh-mirror   # installs /usr/local/bin/gh-mirror
grm update                      # picks up new releases
```

Archives are also on [GitHub releases](https://github.com/jsnjack/gh-mirror/releases).
Builds exist for Linux and macOS on amd64 and arm64. Each archive holds the executable
and this README; `checksums.txt` lists SHA-256 sums. Go is not needed at runtime.

To build from source, install Go 1.26.4 or newer and `monova`:

```sh
make build   # writes bin/gh-mirror, CGO disabled; run it as ./bin/gh-mirror
```

Don't copy a source build into a directory that comes earlier in `PATH` than grm's
install. It hides the grm version.

Install shell completion. It suggests repositories, local label names, catalog kinds
and fixed flag values:

```sh
gh-mirror completion bash > ~/.local/share/bash-completion/completions/gh-mirror
gh-mirror completion zsh  > "${fpath[1]}/_gh-mirror"
gh-mirror completion fish > ~/.config/fish/completions/gh-mirror.fish
```

### Commands at a glance

| Group | Commands |
| --- | --- |
| **Query** | `search`, `list`, `show`, `comments`, `context`, `related`, `catalog` |
| **Collect and publish** | `sync`, `index`, `snapshot`, `acquire` |
| **Serve** | `mcp`, `serve` |
| **Inspect** | `status`, `doctor`, `scope`, `bots`, `model`, `config` |
| **Advanced** | `evaluate` |

- **Output format:** text when stdout is a terminal, JSON when piped. Override with
  `--format text` or `--format json`.
- **JSON is complete:** text output shows compact previews; JSON keeps every field.
- **Another database:** `--db PATH` points any command at another mirror or snapshot.

## Configure

The settings file is `~/.config/gh-mirror/config.toml`. Only TOML is read.
[examples/config.toml](examples/config.toml) lists every setting with its default.
A typical file is short:

```toml
default_repo = "owner/repository"
workers = 8

[pr_comments]
exclude_known_ai = true
exclude_ai_threads = true

[repos."owner/repository"]
preset = "all"
pull_request_review_comments = false

[repos."owner/code"]
preset = "pull_requests"
```

Omitted settings keep their defaults:

| Key | Default | Meaning |
| --- | --- | --- |
| `default_repo` | the only configured repository | Repository for `#123` references and `catalog` |
| `database` | `~/.local/share/gh-mirror/state.sqlite` | Mirror database |
| `snapshot_dir` | `~/.local/share/gh-mirror/snapshots` | Published snapshots |
| `workers` | 4 | Parallel GitHub requests and CPU index workers (1–16) |
| `max_requests` | 3000 | Request budget per sync invocation |
| `enrichment_interval` | `1h` | Refresh of catalogs, fields, relationships and projects |
| `reconcile_interval` | `24h` | Full inventory that removes deleted and transferred tickets |
| `stale_after` | `1h` | Collection age at which `--ensure-fresh` syncs |
| `ensure_fresh` | `false` | Queries and `status` sync a stale mirror first |
| `token_env` | `GITHUB_TOKEN` | Environment variable holding the GitHub token |
| `api_token_env` | `GH_MIRROR_API_TOKEN` | Environment variable holding the REST/MCP HTTP token |
| `listen` | `127.0.0.1:8787` | `serve` address |
| `api_url`, `graphql_url` | api.github.com | GitHub Enterprise endpoints |

- **XDG variables apply** to the default paths.
- **`--config` and `--db`** override the settings file and database for one command.
- **Tokens never enter the database.** gh-mirror reads them from the environment only.

```sh
gh-mirror config show               # compact TOML
gh-mirror config show --effective   # every default and resolved switch
```

### Repositories and presets

Each `[repos."owner/name"]` table selects a preset. Switches set to `true` or
`false` override it:

| Preset | Enables |
| --- | --- |
| `all` | All eleven switches |
| `issues` | Issues, issue comments, labels, milestones, issue types, fields, relationships, projects |
| `pull_requests` | PR records, PR discussion comments, inline review comments |

| Switch | Collects |
| --- | --- |
| `issues` | Open and closed issues with their raw payloads |
| `pull_requests` | Open and closed PR records from the issue listing |
| `issue_comments` | Issue discussion comments |
| `pull_request_comments` | PR discussion comments |
| `pull_request_review_comments` | Inline review comments with diff context and replies |
| `labels` | Label catalog, including unused labels |
| `milestones` | Milestone catalog, including closed milestones |
| `issue_types` | Organization issue type catalog |
| `fields` | Native issue field definitions and ticket values |
| `relationships` | Parent, sub-issue, blocked-by and blocking links |
| `projects` | Owner project catalog and active or archived memberships |

- **Comments need their ticket kind.** Issue types, fields and relationships need issues.
- **Raw tickets always keep** titles, bodies, state, authors, assignees, labels,
  milestone and type. Turning a catalog off does not remove them.
- **A table without `preset`** uses the global `fields`/`projects` defaults, with inline review comments off.
- **`gh-mirror scope`** prints the resolved switches without contacting GitHub.

Some data is never collected:

- **Pull request internals:** commits, file patches, check runs and approval reviews.
- **Other GitHub content:** reactions, attachments and Discussions.
- **Project details:** project cards and project field values.

### Token permissions

The token needs read access to the configured repositories, their issues, organization
issue types and fields, and owner projects.

- **Enabled resources must be readable.** A 403, 404 or GraphQL error fails the sync;
  disable the switch instead.
- **Personal repositories** have no organization issue fields.
- **User-owned Projects endpoints** need a classic PAT. Fine-grained and installation
  tokens are not supported there.

## Query

Queries read the local database and need no token.

| Command | Purpose |
| --- | --- |
| `search WORDS` | Rank tickets by words or meaning, with source evidence |
| `list` | List tickets by filters, without search words |
| `show REF...` | Read up to 100 tickets, without comments |
| `comments REF` | Page through discussion and review comments |
| `context REF` | Read bounded evidence for one ticket, for an agent or reviewer |
| `related REF` | Find related tickets and duplicate candidates |
| `catalog KIND [SCOPE]` | Read labels, milestones, issue types, issue fields or projects |

`REF` takes three forms:

- **`owner/repo#123`**
- **A GitHub URL** such as `https://github.com/owner/repo/pull/456`.
- **`#123` or `123`**, resolved against `default_repo`.

```sh
gh-mirror search 'socket timeout' --state closed --labels-all bug
gh-mirror show '#123' https://github.com/owner/repository/pull/456
gh-mirror comments '#123' --kind review --limit 20
gh-mirror context '#123' --query 'session continuation'
gh-mirror related 123 --include-comments
gh-mirror catalog labels
gh-mirror catalog projects owner
```

- **Summaries by default:** every query command returns summaries. `--view full` adds
  raw GitHub records.
- **`show` text view** lists title, state, labels, assignees, milestone, type, URL,
  dates, comment count and the start of the body.
- **Older command names still work.** `get`, `batch` and `candidates` are hidden and
  print a notice naming their replacement.

### Search

Search covers titles, bodies, labels, discussion comments and inline reviews in open
and closed tickets. The default engine is hybrid: it combines exact words with
similarity in meaning. See [Semantic and hybrid search](#semantic-and-hybrid-search).

```sh
gh-mirror search 'Acme timeout' --match all --count --facets
gh-mirror search 'socket timeout' --match phrase --in body --in comments
gh-mirror search 'auth' --prefix --in title --exclude-word deprecated
gh-mirror search 'timeout' --repositories owner/support --repositories owner/code
gh-mirror search 'ECONNRESET' --engine lexical
```

| Option | Effect |
| --- | --- |
| `--match any` | Any word matches (default) |
| `--match all` | Every word appears somewhere in the ticket, across sources |
| `--match phrase` | Consecutive words in one source |
| `--prefix` | Match word prefixes; in phrase mode, only the last word |
| `--in` | Restrict to `title`, `body`, `labels`, `comments` or `reviews`; repeat |
| `--exclude-word` | Drop tickets containing the word; repeat |
| `--evidence-limit` | Matching sources per ticket, 1–10 (default 3) |
| `--engine` | `hybrid` (default), `lexical` or `semantic` |

- **Literal matching:** punctuation splits words, and query operators are plain words.
- **Lexical limits:** at most 32 terms and 16,384 bytes. Larger queries fail.
- **Literal modes pick lexical.** `--match all`, `--match phrase` and `--prefix` use the
  lexical engine when no engine is given.
- **Each match** has title, URL, score, a `summary` of metadata and `evidence`.
  Evidence lists the field, excerpt, source URL and comment ID. Review evidence adds
  file, line and up to 1,024 characters of diff.
- **Scores rank results within one query.** Higher is better for every engine. They are
  not percentages or duplicate probabilities.
- **Lexical weights:** titles and label names count five times more than body and comment text.

### Filters, ordering and paging

`search`, `list` and `related` share these filters. REST and MCP use the field names:

| CLI flag | REST/MCP field | Meaning |
| --- | --- | --- |
| `--repo` | `repo` | One repository (search and list) |
| `--repositories` | `repositories` | Any of these repositories; repeat |
| `--state`, `--kind`, `--type` | `state`, `kind`, `type` | Open/closed, issue/pull_request, native type (search and list) |
| `--labels-all` | `labels_all` | Every label; repeat |
| `--labels-any` | `labels_any` | Any of the labels; repeat |
| `--exclude-label` | `exclude_labels` | None of the labels; repeat |
| `--author` | `author` | Ticket author login |
| `--assignee` | `assignees` | Any of the assignees; repeat |
| `--milestone` | `milestone` | Milestone title or number |
| `--created-after`, `--created-before` | `created_after`, `created_before` | Creation bounds, inclusive |
| `--updated-after`, `--updated-before` | `updated_after`, `updated_before` | Update bounds, inclusive |
| `--project` | `project` | Project membership `owner/number`, archived included (search and list) |
| `--field 'Priority=High'` | REST `field=Priority=High`, MCP `field_values` | Native field value; repeat |

- **Dates** accept RFC3339, `YYYY-MM-DD` or an age such as `12h`, `7d` or `2w`.
  A date covers the whole UTC day.
- **Filters combine with AND.** Values inside `--assignee` and `--labels-any` combine with OR.
- **Arrays:** REST repeats the parameter; MCP takes JSON arrays. MCP `field_values` is
  a list of `{"name": "Priority", "value": "High"}`.

```sh
gh-mirror list --labels-all 'client:Acme' --updated-after 7d --sort updated
gh-mirror list --labels-all 'client:Acme' --limit 100 --cursor "$next_cursor"
```

| Ordering | Default |
| --- | --- |
| `list` | Repository and number, ascending |
| `search`, `related` | Relevance, best first |
| `--sort updated` or `created` | Newest first |
| `--sort number` | Ascending |

- **Paging:** results include `has_more` and `next_cursor`. Pass the cursor back with
  the same options; `--limit` may change.
- **Stale cursors:** a new sync, index or policy changes the generation. The old cursor
  then fails with a stale-cursor error, and the query must start again.
- **`--count`** adds `total`. **`--facets`** adds label, type and project counts,
  up to 100 values each.
- **Warnings** report uncollected repositories, disabled resources and stale fields or
  projects. `--max-enrichment-age` sets the threshold (default 24h).

### Tickets and comments

- **`show`** reads up to 100 tickets from one generation. Duplicates are merged; unknown
  tickets appear under `missing`.
- **`comments`** pages through `--kind all|discussion|review` comments in creation order.
- **Previews** stop at 1,024 characters and set `truncated`. `--view full` returns
  complete text and raw JSON.
- **Comment IDs are strings,** so large numeric IDs survive JSON parsers.
- **Policy exclusions** are counted in `excluded_by_policy` for the selected kind.
  `--include-excluded` returns those comments.

### Related tickets

`related` builds a query from the seed ticket and returns similar history, including
closed tickets. The seed itself is excluded.

```sh
gh-mirror related owner/support#123 --include-comments --count
gh-mirror related owner/support#123 --repositories owner/support --repositories owner/code
```

- **Seed terms:** up to 24 distinctive terms, with titles weighted above bodies.
  Acronyms and error codes such as CSS, API and 403 are kept.
- **Labels count by default.** `--include-labels=false` drops them.
- **Comments are opt-in.** `--include-comments` adds recent seed comments, bounded by
  `--comment-limit` (default 20, maximum 100).
- **Scope:** the seed repository by default; `--repositories` widens it.
- **Mutual similarity counts.** Hybrid ranking favours tickets that also rank the seed
  among their own nearest neighbours, which finds duplicates worded differently.

### Ticket context

`context` returns one ticket's evidence in a bounded size, for an agent or reviewer.

```sh
gh-mirror context owner/repository#123 --query 'session continuation'
gh-mirror context owner/repository#123 --purpose proofread --max-characters 20000
```

| Purpose | Use |
| --- | --- |
| `evaluate` (default) | General assessment |
| `classification` | Choosing labels; adds label definitions and rules |
| `duplicates` | Checking whether two tickets describe the same problem |
| `proofread` | Rewriting the description; gives the body most of the budget |

The result includes:

- **The description** with section byte offsets and absent or empty sections.
- **Label roles** such as client or feature, plus label descriptions.
- **Closure and merge metadata.**
- **Selected comments:** ones matching `--query`, plus the most recent.
- **Related tickets one hop away:** native relationships, mentions, and explicit
  duplicate or fix references.

Size and parsing:

- **Bounds:** `--comment-limit` (default 10), `--related-limit` (default 5) and
  `--max-characters` (default 12,000).
- **Coverage** reports omitted comments, unexpanded relations and cut text.
- **References are observations.** The agent decides whether two tickets describe the same problem.
- **Reference parsing** ignores `#N` after a word character, slash, colon or dot. Version
  strings and URL fragments therefore don't count.

Label conventions such as `client:Acme` and `feature:x` are recognized by default.
Configure others per repository:

```toml
[[label_rules."owner/repository"]]
prefix = "customer:"
role = "client"

[[label_rules."owner/repository"]]
label = "account:example"
role = "client"
value = "Example"
aliases = ["Example Ltd"]
description = "Affected customer"
```

Exact labels take priority over prefixes, and the longest prefix wins. Run `index` or
`sync` to save changed rules into the database. Neither contacts GitHub for this.

### Catalogs

| Kind | Default SCOPE |
| --- | --- |
| `labels`, `milestones` | Default repository |
| `issue_types`, `issue_fields`, `projects` | Owner of the default repository |

`catalog` returns the whole catalog. `--limit` or `--cursor` switch to pages. Unknown
kinds fail with the list of valid ones.

### Investigating a ticket

Use the query commands to find causes, earlier decisions, workarounds and fixes.
Duplicate detection is one case of this.

1. **Search with exact terms first:** error text, component names, client labels.
2. **Keep closed tickets.** They hold most resolutions.
3. **Read the evidence,** then `show` the promising tickets.
4. **Read the discussion** with `comments` or `context` before trusting a snippet.
5. **Cite source URLs** and check `warnings` for stale or missing data.

```sh
gh-mirror search 'workaround for downloads after reconnecting' --limit 10
gh-mirror search 'session token retry fix' --in reviews
gh-mirror show owner/support#123 owner/code#456
gh-mirror comments owner/support#123 --view full
```

Scores order candidates for reading. They do not prove relevance or duplication.
Semantic results near the bottom can be unrelated.

## Sync

```sh
gh-mirror sync      # first run creates the database; later runs update it
gh-mirror status    # database path, generation, scope, counts, timestamps
gh-mirror doctor    # health checks; exits 1 when one fails
```

After collecting, `sync` updates the semantic index and publishes a snapshot.
`--index=false` and `--publish=false` skip those steps.

### Sync only when stale

`--ensure-fresh` is a global flag. It syncs first when the mirror is older than
`stale_after`, then runs the command:

```sh
gh-mirror --ensure-fresh search 'login loop'        # sync if stale, then search
gh-mirror --ensure-fresh --stale-after 15m status   # one-off threshold
gh-mirror sync --ensure-fresh --quiet               # sync only; skip a fresh mirror
```

To make it the default for every query, set it in the configuration:

```toml
stale_after = "30m"
ensure_fresh = true
```

- **`ensure_fresh = true` covers queries and `status`.** These are `search`, `list`, `show`,
  `comments`, `context`, `related` and `catalog`. `--ensure-fresh=false` turns it off for one call.
- **`serve` and `mcp` sync only with an explicit `--ensure-fresh`.** They sync once at startup.
  The configuration key never makes a server write.
- **`sync` ignores `ensure_fresh`.** `sync --ensure-fresh` skips a fresh mirror; plain `sync`
  always collects.
- **Other commands reject `--ensure-fresh`.**
- **A fresh mirror is left alone.** The check sends no GitHub requests and needs no token.
- **A running sync is not joined.** If another process is collecting, the command uses the
  current data at once. An interrupted sync is not running, so it resumes.
- **A stale, uncollected or missing mirror** gets a normal sync, including index and publish.
- **Concurrent calls collect once.** The others report `running`, or `fresh` after it commits.
- **A failed sync keeps the old data.** The command prints a warning on stderr and reads the
  previous collection. It fails only when the mirror has never been collected.
- **stdout carries only the command result.** Sync progress shows on stderr only when stderr is
  a terminal.
- **`--stale-after` needs `--ensure-fresh`** (flag or configuration) and must be positive.

A skipped `sync --ensure-fresh` prints one result instead of the sync report:

```json
{"skipped":true,"reason":"fresh","collected_at":"2026-10-15T09:37:00Z","age":"23m0s","stale_after":"1h0m0s"}
{"skipped":true,"reason":"running","collected_at":"2026-10-15T07:02:00Z","age":"2h58m0s","stale_after":"1h0m0s"}
```

`collected_at` and `age` are absent when the mirror has never been collected.

### What each sync fetches

| Data | When |
| --- | --- |
| Changed issues and comments | Every sync |
| Catalogs, fields, relationships, projects | Every `enrichment_interval` (1h) |
| Full inventory, removing deleted and transferred tickets | Every `reconcile_interval` (24h) |
| Everything | `sync --full` |

- **Unchanged repositories cost two requests:** one issue listing and one comment
  listing. Inline review comments add a third. A 304 response still counts.
- **Comments are polled separately,** so edits to comments on old issues arrive too.
- **Deletions are confirmed first.** A ticket missing from a full listing is fetched
  directly. Sync keeps it if GitHub still returns it.
- **Renamed or deleted labels** make the next enrichment refetch the tickets that carried them.
- **A changed token or API host** triggers one full reconciliation. A permission change
  on the same token needs `--full`.
- **Data format upgrades** (`collection_version`) rebuild automatically. The old mirror
  stays readable until the rebuild commits.

### Interruptions and failures

- **Ctrl-C is safe.** Rerun `sync` to resume. Completed responses are saved in
  `<database>.sync.sqlite` and cost no requests on resume.
- **A failed sync changes nothing.** Readers keep the last complete generation.
- **Failures are recorded.** `status` and `doctor` show the attempt count and last error
  while unfinished work exists.
- **Repeated failures still reach reconcile,** because intervals use the current time.
- **`sync --restart`** discards unfinished work.
- **Keep the database directory on persistent storage,** including the resume file.

### Workers, budget and rate limits

```sh
gh-mirror sync --workers 8                    # GitHub requests and indexing
gh-mirror sync --workers 8 --index-workers 4  # cap CPU indexing separately
gh-mirror sync --workers 1                    # strictly serial requests
```

- **Workers** range from 1 to 16 (default 4). They share one request budget and one
  rate-limit pause.
- **Parallel and serial syncs fetch the same pages.** There are no speculative requests.
- **Secondary rate limits** wait at least one minute and back off for up to five attempts.
- **An exhausted quota** waits until GitHub's reset time. Waits over 61 minutes stop the sync.
- **The budget** (`max_requests`, default 3000) applies per invocation. Stopping at it
  keeps completed work for the next run.
- **Serial mode** follows GitHub's advice for avoiding secondary limits.

### Progress

Progress goes to stderr; stdout keeps the JSON result. In a terminal it shows the
phase, pages, records, request budget, cache hits and remaining quota:

```text
gh-mirror sync | elapsed 12s
⠹ Hydrating issue metadata
  ━━━━━━────────────── 100/300 (33%) | 25.0/s | phase ETA 8s
  Received: 300 issues, 840 comments | repository 1/1
  API: 19/3000 requests | 0 cached | GitHub workers 4/4 active
  GitHub remaining: 4981/5000
  Resume: 8 resumed responses | 10 saved before this run
```

`cached` counts 304 Not Modified answers. GitHub does not charge quota for them. A
finished sync lists committed changes per repository, which the JSON result also
reports under `changes`:

```text
✓ Sync complete | elapsed 2m41s
  surfly/it: issues +3 ~41 -1 | comments +88 -2
  surfly/cobro: pull requests +12 ~30 | comments +240
  + added, ~ updated, - removed
```

Colours mark the phase, bar, retry waits, quota below 25% and 10%, and the outcome.
`--color auto` (default) colours terminals unless `NO_COLOR` is set; `always` and
`never` override it. Redirected stderr and `--debug` print plain status lines
instead. `--quiet` hides progress.

### Scheduling

Run one collector at a time, with `GITHUB_TOKEN` in its environment:

```cron
*/15 * * * * /usr/local/bin/gh-mirror --config /etc/gh-mirror/config.toml sync --quiet
```

`serve` and `mcp` only read. They sync once at startup only with an explicit `--ensure-fresh`.

### Upgrading

- **Schema 3 needs a current binary.** Older binaries fail with "unsupported database schema".
- **Parser or policy version changes** re-index locally on the next `sync` or `index`.
  This needs no GitHub requests.
- **Changed listing order:** issues and comments are now listed by creation time. The
  first sync after upgrading cannot reuse cached listing responses or saved listing pages.

## Snapshots

A snapshot is a standalone, read-only copy of one complete generation, including its
search index. `sync` publishes one after each successful run. To publish manually:

```sh
gh-mirror snapshot
```

- **Never copy the live database.** It uses WAL sidecars and changes during sync.
  Snapshots are self-contained.
- **Collector data is stripped.** Response caches, the credential hash and resume data
  are removed, and the file is compacted.
- **Publishing is atomic.** The file gets a unique name and is fsynced before
  `latest.json` points to it.
- **The three newest files are kept.** Older snapshots are deleted on publish.
- **Many readers can share one snapshot.** Readers open it read-only and write nothing.

### Use a snapshot on the same host

Copy the file `latest.json` names, then query the copy:

```sh
SNAPSHOTS=~/.local/share/gh-mirror/snapshots
cp "$SNAPSHOTS/$(jq -r .filename "$SNAPSHOTS/latest.json")" ./mirror.sqlite
gh-mirror --db ./mirror.sqlite doctor --max-age 2h
gh-mirror --db ./mirror.sqlite search 'socket timeout'
```

Copy rather than read the published file in place. Pruning can delete it during a
long job.

### Fetch a snapshot from another host

`acquire` downloads a snapshot from a shared path or an HTTPS `serve` endpoint:

```sh
gh-mirror acquire --source /srv/gh-mirror/snapshots/latest.json \
  --dest ./mirror.sqlite --repo owner/repository --max-age 2h > mirror-manifest.json

gh-mirror acquire --source https://mirror.example/snapshots/latest \
  --dest ./mirror.sqlite --repo owner/repository
```

It checks the SHA-256, exact repository scope, schema and collection versions, and
age. Then it installs the file atomically.

- **`--max-age`** limits issue and comment age (default 2h).
- **`--max-enrichment-age`** separately limits field and project age (off by default).
- **A failed acquire** leaves the previous destination untouched.
- **Downloads** are limited to 2 GiB.

## MCP

Point an MCP client at the executable:

```json
{
  "mcpServers": {
    "gh-mirror": {
      "command": "/usr/local/bin/gh-mirror",
      "args": ["--db", "/path/to/mirror.sqlite", "mcp"]
    }
  }
}
```

`serve` also exposes Streamable HTTP MCP at `http://127.0.0.1:8787/mcp`, using the REST
authentication. Every tool is read-only.

| Tool | Set | Purpose |
| --- | --- | --- |
| `search_issues` | core | Search with filters, evidence and cursors |
| `find_duplicate_candidates` | core | Related tickets for a seed ticket |
| `get_issue_context` | core | Bounded evidence for one ticket |
| `get_issues` | core | Up to 100 tickets, without comments |
| `get_issue` | core | One ticket; summary without comments by default |
| `list_comments` | core | A page of comments |
| `get_sync_status` | core | Generation, scope, coverage and freshness |
| `list_issues` | all | Filtered listing without search words |
| `get_catalog` | all | A complete catalog |
| `list_catalog` | all | A catalog page |
| `get_project` | all | A project record |

- **Core tools by default.** `mcp --tools all` or `serve --mcp-tools all` adds the rest.
- **Every input is described,** and fixed choices list their allowed values.
- **Results stay small.** Their `status` holds only `generation` and `collected_at`;
  `get_sync_status` returns the full status.
- **`get_issue` comments:** `comments=page` returns one bounded page; `comments=all`
  returns every comment.
- **Engine:** `search_issues` and `find_duplicate_candidates` accept `engine`. Read
  `query.engine`, `ranking` and `warnings` for what actually ran.

## REST API

```sh
gh-mirror serve                       # listens on 127.0.0.1:8787
curl 'http://127.0.0.1:8787/v1/search?q=socket+timeout&repo=owner/repository'
curl 'http://127.0.0.1:8787/v1/issues/owner/repository/123?view=summary'
curl -X POST 'http://127.0.0.1:8787/v1/issues/batch' -H 'Content-Type: application/json' \
  --data '{"tickets":[{"repo":"owner/repository","number":123}],"view":"summary"}'
```

| Endpoint | Purpose |
| --- | --- |
| `GET /health` | Health check |
| `GET /v1/status` | Generation, scope, coverage and freshness |
| `GET /v1/search?q=...` | Search with the shared filters, `engine`, `match`, `prefix`, `in`, `exclude_words` |
| `GET /v1/issues` | Filtered listing |
| `GET /v1/issues/{owner}/{repo}/{number}` | One ticket; `view`, `comments`, `limit`, `cursor`, `comment_kind` |
| `GET /v1/issues/{owner}/{repo}/{number}/comments` | Comment page; `kind`, `view`, `order`, `limit`, `cursor` |
| `GET /v1/issues/{owner}/{repo}/{number}/context` | Ticket context; `purpose`, `query`, limits |
| `GET /v1/issues/{owner}/{repo}/{number}/related` | Related tickets with the shared filters |
| `POST /v1/issues/batch` | Up to 100 tickets; JSON `tickets` and optional `view` |
| `GET /v1/catalog?kind=...&scope=...` | Catalog; `limit` or `cursor` for pages |
| `GET /v1/projects?owner=...&number=...` | Project record |
| `GET /snapshots/latest`, `/snapshots/{filename}` | Snapshot manifest and files |

`/v1/candidates?repo=...&number=...` remains as an alias of `/related`.

| Status | Code | Cause |
| --- | --- | --- |
| 400 | `invalid_query` | Invalid input |
| 404 | `not_found` | Unknown ticket or resource |
| 409 | `stale_cursor` | Cursor from an older generation |
| 503 | `semantic_index_unavailable` | Explicit semantic/hybrid request without a complete index |
| 500 | `internal_error` | Internal failure |

- **Authentication:** set `GH_MIRROR_API_TOKEN` and send `Authorization: Bearer <token>`.
- **A token is required off loopback.** Once set, every endpoint requires it, loopback included.
- **Use a TLS proxy** to expose the server remotely.

## Semantic and hybrid search

The executable bundles the MiniLM L6 model, about 91 MB, and runs it in Go on the CPU.
The first use unpacks it into `~/.cache/gh-mirror/models/`. `gh-mirror model` prints
the model identity.

| Engine | Ranks by | Index needed |
| --- | --- | --- |
| `lexical` | Literal words (SQLite FTS5 BM25) | No |
| `semantic` | Meaning only (cosine similarity) | Yes |
| `hybrid` (default) | Words, meaning and whole-ticket evidence | Yes |

- **No index, no error by default.** Without a complete index, an omitted engine falls
  back to lexical with a `search_engine_fallback` warning.
- **Explicit engines are strict.** `--engine semantic` or `hybrid` fail without a complete
  index; REST returns 503.
- **Result size:** semantic keeps the 200 most similar tickets. Hybrid adds every literal
  and ticket-evidence match. Totals and facets count that set.
- **Hybrid score:** `1/(60+lexical rank) + 1/(60+semantic rank) + 3/(60+ticket rank)`.
  Ticket evidence rewards distinctive terms across fields and soft client-label matches.
- **Filters apply to every engine.** Label filters are hard limits; client matches only
  boost the score.
- **Limits:** 16,384 bytes and 16 passages per query; hybrid allows 512 literal terms.
- **English-oriented model.** Hybrid keeps exact client labels and technical terms.
  The [model research](docs/semantic-model-research.md) compares alternatives.

### Index

`sync` indexes new and changed text after each run. Build or resume the index alone,
without GitHub requests:

```sh
gh-mirror index --workers 8
gh-mirror search 'file transfers get stuck indefinitely' --engine semantic
```

- **Passages:** titles, bodies, labels, comments and reviews are indexed separately. Long
  text splits into 256-token passages with small overlaps.
- **Body passages skip** empty template sections and review checklists. Offsets still
  point into the original text.
- **Incremental:** unchanged documents are skipped. Edited documents reuse identical
  passages, and identical text shares one vector.
- **Resumable:** completed passages are saved immediately. Ctrl-C and rerun.
- **Snapshots carry the index,** so consumers search offline without rebuilding it.
- **After an interrupted index,** run `index` again, then `snapshot` to publish.

### Vulkan acceleration

With Lemonade installed, its Vulkan runtime can encode text on the GPU:

```sh
lemonade --no-discovery backends install llamacpp:vulkan
gh-mirror model --check --embedding-backend lemonade-vulkan --format text
gh-mirror index --embedding-backend lemonade-vulkan
```

Make it the default:

```toml
[embedding]
backend = "lemonade-vulkan"
batch_size = 8        # 1–32
device = "Vulkan0"
```

- **Dedicated process:** gh-mirror starts its own localhost `llama-server`, offline
  and authenticated. It stops with gh-mirror, also when gh-mirror is killed on Linux.
- **The API key stays private.** It is passed in a temporary file, not on the command line.
- **Same model:** an exact FP32 GGUF export of the bundled weights, about 87 MiB of
  extra cache. No Python tools are needed.
- **Verified output:** GPU vectors must match CPU reference vectors (cosine ≥ 0.9999,
  coordinate error ≤ 0.001). Otherwise gh-mirror falls back to the CPU and says why.
- **Interchangeable:** CPU and Vulkan vectors are compatible; switching keeps the index.
- **Runtime path:** `embedding.runtime` or `--embedding-runtime` overrides the default
  Lemonade cache location.
- **Benchmarks:** see the [Vulkan benchmark](docs/vulkan-benchmark-2026-10-10.md).

## AI review comment filtering

To keep AI reviewer comments out of search, context and the semantic index:

```toml
[pr_comments]
exclude_known_ai = true      # Copilot, CodeRabbit, Gemini Code Assist, Claude
exclude_ai_threads = true    # also replies under an excluded inline review
exclude_all_bots = false     # every bot, including coverage and dependency bots
exclude_authors = ["custom-reviewer"]
include_authors = ["trusted-reviewer[bot]"]

[repos."owner/code".pr_comments]
exclude_known_ai = false     # per-repository override
```

- **Raw comments are kept** in the mirror and snapshots; only retrieval skips them.
- **Detection uses stored data:** known bot accounts from the
  [registry](internal/commentpolicy/registry.md) and explicit attribution lines.
  Example: `Addressed by [Claude Code](https://claude.com/claude-code)`.
- **Declare AI comments explicitly** by ending them with `<!-- gh-mirror:ai-generated:claude -->`.
- **Quotes, code and mentions are ignored.** Unmarked human comments stay included.
- **`include_authors` takes priority,** even inside excluded threads.
- **Only PR comments are filtered.** Issue comments and PR descriptions stay searchable.
- **Apply changes** with `index` or `sync`. Readers use the policy saved in their database.
- **Repository overrides** inherit global settings. A given author list replaces the
  inherited one; an empty list clears it.

```sh
gh-mirror bots --format text                       # detected accounts and exclusion counts
gh-mirror comments owner/code#123 --include-excluded
```

## Evaluate retrieval

Measure retrieval against your own relevance judgments:

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
gh-mirror --db ./mirror.sqlite evaluate --cases ./judgments.json --format text
```

- **Metrics:** Precision@10 (relevant hits divided by 10) and Recall@20, with p50 and
  p95 latency.
- **Judge every relevant ticket.** Unjudged results count as misses and cap Precision@10.
- **Pinned generation:** evaluation fails if the mirror changes during the run.
- **Benchmarks:** see the [measured comparison](docs/benchmark-2026-10-10.md) of the three engines.

## Development

```sh
make check      # goimports, vet, build, race tests, golangci-lint
make release    # build archives and publish a GitHub release
```

- **Tools:** checks need `goimports`, `golangci-lint` and `monova`. The Makefile prints
  install commands for missing ones.
- **Releases:** `make release` needs `grm` and a clean, pushed `main`. It uploads the
  archives and checksums to a new release tagged `v` plus monova's version.
- **Specification:** [docs/spec.md](docs/spec.md).
- **Synthetic retrieval tests:** `go test ./internal/store -run TestRetrievalEvaluation -v`.

### Diagnostics

- **`--debug`** prints request diagnostics on stderr.
- **`--trace`** writes detailed metadata to `gh-mirror-<uid>.log` in the temporary
  directory. The file refuses symbolic and hard links.
- **No secrets in logs.** Diagnostics never include tokens or ticket bodies.

### GitHub API references

- **Listings:** [repository issues](https://docs.github.com/en/rest/issues/issues#list-repository-issues)
  and [repository comments](https://docs.github.com/en/rest/issues/comments#list-issue-comments-for-a-repository),
  REST version `2026-03-10`.
- **Metadata:** [native issue GraphQL schema](https://docs.github.com/en/graphql/reference/issues)
  and [Projects](https://docs.github.com/en/rest/projects/projects).
- **Rate limits:** [rate-limit guidance](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api)
  and [best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api).
- **GitHub Enterprise Server** may lack features. Enabled features that are missing fail
  the sync instead of returning empty catalogs.
