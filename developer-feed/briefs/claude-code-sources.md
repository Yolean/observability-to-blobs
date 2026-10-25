# Brief: what of Claude Code's own files the feed collects

For a dedicated Claude Code session in this repository, on the `developer-feed` branch, under
`developer-feed/CLAUDE.md`. Written 2026-10-25 from a review on the machine `smack3`. The
developer has not yet decided on the proposals below; ask before building them.

## Settled requirements

These are the developer's decisions. Don't revisit them.

- **What the feed must hold** for Claude Code:
  - every prompt the developer wrote;
  - the files produced;
  - the git commits Claude made, and the remotes and branches they were pushed to, by
    anyone, as far as the local clone knows.

  The purpose is to document what requirements went into each feature, across tens of
  repositories. When Claude formalises open points (questions, plans), that is especially
  wanted. So are the memories agents record.
- **Lines are shipped verbatim** and only appended. Redaction is for later tools, before
  anything is published.
- **Collectors are live only.** History from before the agent's first start is the backfill's:
  `dev-feed-backfill`, a separate binary, never the engine.
- **The backfill must take on a category the feed has never had**, even when every other
  category is backfilled already. Built: per-category state, `backfill/README.md`.
- **Local retention is a feature** once forwarding runs: Claude Code's own cleanup, 30 days by
  default, may delete what has been shipped.
- **No secret goes into any file**, and none may be shipped.
- **The developer's real bucket is never used for development.** Test against versitygw
  (`itest/run.sh`).
- **A production node's footprint and security surface stay as they are** unless a collector
  is enabled there. The feed's collectors run only from `developer-feed/collectors/`.

## Current scope and status

What the agent collects from `~/.claude`, every line verbatim (`collectors/collectors.d/`):

| Collector | Files |
|---|---|
| `claude` | `projects/*/*.jsonl` (sessions), `projects/*/*/subagents/*.jsonl` (subagents), `history.jsonl` (prompts) |
| `claude-memory` | `projects/*/memory/*.md`, as snapshots |

The backfill has a category for each. Explore keeps every line it ingests; its views pick
fields.

What is not collected, as found on 2026-10-24 (counts on smack3):

| Path under `~/.claude` | Files | What | Assessment |
|---|---|---|---|
| `projects/*/<session>/subagents/workflows/<run>/*.jsonl` | 66 | workflow subagents' transcripts | a gap: the subagent glob stops one level too early |
| `projects/*/<session>/workflows/*.json`, `workflows/scripts/*.js` | 3 + 3 | a workflow run: summary, result, phases, token and tool counts, and its script | wanted: the record of a multi-agent run |
| `projects/*/<session>/tool-results/*.txt` | 110 | the whole output of a large tool result; the transcript has a 2 KB preview and this path | wanted: long command output, fetched documents |
| `projects/*/<session>/subagents/*.json`, `.../workflows/<run>/*.json` | 146 + 63 | a subagent's `agentType`, `description`, `spawnDepth`, ... | minor: the parent's tool call says most of it |
| `file-history/<session>/<hash>@v<n>` | 1,355, 17 MB | whole versions of the files Claude edited | undecided: transcripts have Write contents and Edit diffs, not each whole file |
| `plans/*.md` | 4 | plans | minor: the plan text is in the `ExitPlanMode` tool call |
| `paste-cache/*.txt` | 45 | larger pastes in history prompts (106 history entries point to one) | only for prompts whose transcript is gone; transcripts have the paste inline |
| `projects/*/<session>/tool-results/*.pdf`, `*.jpg` | ~76 | fetched PDFs, pages rendered as images | no: binary |
| `sessions/` | 24 | `peerToken` | **never ship**: credentials |
| `shell-snapshots/` | 10 | shell environments | **never ship**: can hold secrets |
| `plugins/`, `cache/`, `backups/`, `daemon/`, `jobs/` | | Claude Code's housekeeping | no |

The longest transcript line is 2.6 MB, so the 16 MB line limit (`buffer_max_size`) drops
nothing today.

## Proposed, not decided

1. **Workflow subagents:** a tail input in `collectors.d/claude.yaml` for
   `projects/*/*/subagents/workflows/*/*.jsonl`, with `session_id` and `project` from the path,
   and a backfill category. Decide whether its `kind` is `subagent` or a new one.
2. **Workflow runs, tool results, subagent metadata:** snapshots with `dev-feed-snapshot`, as
   `claude-memory` is, each with a backfill category. These files are written once, so each
   ships once. Decide the `kind` names and whether the feed's line contract (`sql/feed.sql`)
   needs a column for them.
3. **`file-history/`:** whether at all. It is the only source of whole file versions.

## What an implementation must keep

- **fluent-bit v5.1.2:** it refuses a list of more than 10 items in a processor's
  configuration (`collectors.d/git.yaml` explains), and a tail glob does not recurse.
- **Every new collector:**
  - the e2e (`itest/run.sh`): live only, verbatim, once each;
  - a backfill category (`backfill/categories.go`), with a check that a line shipped by both
    the agent and the backfill gets the same fields;
  - explore views (`explore/sql/`), and the dedupe identity in `explore/sql/001-ingest.sql`.
- **Docs:** `README.md` (Collectors), `backfill/README.md` (Categories) and
  `explore/ANALYZE.md`.
