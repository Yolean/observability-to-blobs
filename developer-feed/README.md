# developer-feed

A developer's own feed: what their machines know about their work, shipped by the host agent
to a bucket of their own, which they use as their own datalake and may publish artifacts
from. Nobody is asked to run it; developers try it and keep it if it helps their work.

This tree lives on the long-lived branch `developer-feed`, rebased onto `main`:

- **No pull request from the branch,** and **no `v*` tag on it.** CI runs on pushes to `main`,
  on pull requests and on `v*` tags (`.github/workflows/`), so the branch runs no GitHub
  Actions. It is built and tested on the developer's machines.
- **What should go upstream** gets its own topic branch off `main` and a normal pull request
  with `./test.sh`. Candidates: the modular collectors, metrics shipped from the host build,
  and an upload hold for `out_s3`.
- **What stays here** is this directory. The branch adds files rather than editing shared ones,
  so rebases stay cheap.

Development and end-to-end tests run against versitygw in docker, the way `itest/host/` tests
the host agent, never against a developer's real bucket. A PutObject-only user there stands in
for the minted R2 credential.

All machines of one developer write to one bucket, in the host agent's key layout
(`HOST_MODE.md`):

```
/{KEY_ROOT}/{CLUSTER_NAME}/{YYYY-MM-DD}/{HH}/{NODE_NAME}/{MMSS}-{uuid}.jsonl.zst
```

- `KEY_ROOT` is `dev1`.
- `CLUSTER_NAME` and `NODE_NAME` name the machine: for the user-level agent each machine is a
  cluster of its own. The feed is the bucket.

| Directory | What |
|---|---|
| [`cloudflare/`](cloudflare/) | `dev-feed-r2`: write-only and read credentials for an R2 bucket, minted locally from a parent token per machine, and a check that they are what they claim |
| [`collectors/`](collectors/) | the feed's collectors, a directory for the host agent's `HOST_COLLECTOR_DIRS` (`HOST_MODE.md`, Collectors) |
| [`git/`](git/) | `dev-feed-git`, the git collector's command |
| [`bin/`](bin/) | `dev-feed-cmux`, the cmux collector's command |
| [`backfill/`](backfill/) | `dev-feed-backfill`: the history from before the collectors ran, per category |
| [`examples/`](examples/) | your own source: a collector in fluent-bit configuration and its explore schema, for the config (`macos/README.md`, Your own sources) |
| [`briefs/`](briefs/) | briefs for a dedicated session: settled requirements apart from current scope and status |
| [`sql/feed.sql`](sql/feed.sql) | the feed's line contract, for DuckDB |
| [`itest/`](itest/) | the collectors end to end, in docker against versitygw |
| [`macos/`](macos/) | the user-level host agent on a Mac: a native build, the install and LaunchAgent, the credential in the Keychain |
| [`explore/`](explore/) | the feed in DuckDB, ingested from the bucket every few minutes, served with quack on localhost |
| [`lib/config.sh`](lib/config.sh) | the user-level layout (program, `config/v1`, `state/v1`) and the config file the scripts maintain |

## Collectors

The feed's collectors run in the host agent, from `developer-feed/collectors/` named in
`HOST_COLLECTOR_DIRS`. Their commands must be on the agent's `PATH`:

- `developer-feed/bin/`;
- `dev-feed-git`, built with `go build` in `git/`.

All of them are on by default from that directory. The host's own defaults are turned off with
`--no-collector.<name>` in the args file.

| Collector | Reads | Rows |
|---|---|---|
| `claude` | `~/.claude/projects/*/*.jsonl` (sessions), `.../*/subagents/*.jsonl`, `~/.claude/history.jsonl` (prompts) | `claude`: `kind`, `project` and `session_id` from the path, the line verbatim in `json` |
| `git` | every repository in the prompt history's `project`s and in `DEV_FEED_GIT_ROOTS`, every 300 s (`DEV_FEED_GIT_INTERVAL`) | `git`: `remote`, `ref-update` (a reflog entry: a push from here, a fetch of anyone's push, a commit), `commit` (whole message, parents, author, committer) |
| `cmux` | cmux's agent journal, every 60 s (`DEV_FEED_CMUX_INTERVAL`) | `cmux`: a journal row, with the agent's session and the cmux workspace and pane; metadata only |
| `hammerspoon` | `~/Reposoft/productivity-logs/hammerspoon-activity-log.jsonl`, the activity log of the productivity-workspaces Hammerspoon configuration | `hammerspoon`: the line verbatim in `json`: focus, title changes, Spaces, idle, lock and sleep |
| `claude-memory` | `~/.claude/projects/*/memory/*.md`, every 60 s (`DEV_FEED_MEMORY_INTERVAL`) | `claude` with `kind` `memory`: a file's whole `content` when new or changed, `deleted` when gone; the project from the path |

**Every collector but `claude-memory` is live only.** What it finds at the agent's first start
is a position, not something to ship:

- `claude` follows the files that exist at the first start from their ends, and new files
  from their first line.
- `git` records each repository's reflogs and tips, and ships its remotes.
- `cmux` records the journal's newest sequence.

After that each collector resumes where it stopped, across restarts. `claude-memory` ships every
memory file there is on its first run. A memory's content is state rather than history, and
Claude Code's cleanup of old sessions leaves memories alone, so they reach back further than the
transcripts. The history from before the first start is `dev-feed-backfill`'s
([`backfill/`](backfill/)), category by category.

The rows are read through `sql/feed.sql`:

```sql
SET VARIABLE raw_files = 's3://<bucket>/dev1/<cluster>/*/*/*/*.jsonl.zst';
.read developer-feed/sql/feed.sql
SELECT kind, session_id, json FROM feed WHERE "table" = 'claude';
```

**What the rows contain:** `claude` and `git` rows hold free text, verbatim. Prompts, tool
output (which can include secrets a tool printed) and commit messages all go to the
developer's own bucket. Redaction is for the tools that read it, before anything is
published.
