# Analyzing a developer feed: instructions for an agent

You are analyzing one developer's own activity data: their Claude Code sessions, git, cmux
and window focus, from their machines. They want to understand their work and work habits.
Answer from the data, show the queries behind each claim, and say what the data cannot show.

## Access

A running `explore.sh` serves the feed as a DuckDB database. Query it with:

```sh
developer-feed/explore/query.sh "<SQL>"     # or the SQL on stdin
```

The script is next to this file. It needs `DEV_FEED_QUACK_TOKEN` in the environment; the
developer starts you with it. Never print, echo or write the token: refer to the variable
only. The script refuses anything but reads. Do not try to get around that: the database is
explore's, and other people's tools read it.

Times are `TIMESTAMPTZ` and display in the machine's local time zone. Say times to the
developer in that zone.

First, see what is there:

```sql
SELECT "table", kind, cluster, min(ts) AS first, max(ts) AS last, count(*) AS rows
FROM ingest.rows GROUP BY ALL ORDER BY 1, 2;
FROM ingest.runs ORDER BY started_at;   -- explore's ingests: from what hour, of which clusters
```

Coverage is not everything that happened:
- **Live since the agent's first start** on each machine (`cluster`). Before that there is only
  what a backfill shipped (`backfill` is set on those lines, `ingest.rows.backfill`).
- **Only from explore's `<since>`:** explore ingests from that far back. If the developer asks
  about a period before `ingest.files` begins, say that explore needs a longer `<since>`.
- **New lines arrive every few minutes:** explore re-ingests every 5 minutes, and the agent
  uploads its buffer every 30 s or so. The last minutes may be missing.

## Schemas

Every view reads `ingest.rows`, which has each line once. `ingest.all_rows` also has lines
shipped twice, where the agent's and a backfill's ranges meet. `cluster` is the machine.

**`claude`: Claude Code.** The richest source: the developer's words and the agent's work.

| View | What |
|---|---|
| `claude.sessions` | a session: `session_id`, `project` (the cwd with `/` as `-`), `first_at`, `last_at`, `entries`, `title` (Claude Code's own) |
| `claude.prompts` | what the developer typed: `occurred_at`, `session_id`, `project_path`, `git_branch`, `prompt`, `source`. `source` is `transcript` (the session's own record) or `history` (Claude Code's prompt history). Both have most prompts: count a prompt once, by session and text |
| `claude.entries` | every transcript entry: `type` (`user`, `assistant`, `system`, `attachment`, ...), `occurred_at`, `cwd`, `git_branch`, and the whole `entry` JSON |
| `claude.tool_calls` | every tool the agent called, with `input` JSON: `Bash` (`input->>'command'`: git commit, git push, gh pr create), `Write` and `Edit` (`input->>'file_path'`), `AskUserQuestion` (questions to the developer, with options), `ExitPlanMode` (plans) |
| `claude.memories`, `claude.memory_versions` | the memory files agents keep per project, the current ones and every version |

Things that need the `entry` JSON:
- **Tool results:** user entries whose `message.content` holds `tool_result` items with a
  `tool_use_id`. Join them to `tool_calls` for a call's output, such as a commit's sha or a
  test's result.
- **Subagents:** `kind = 'subagent'` entries are their transcripts, with their parent's
  `session_id`.
- **Format:** the transcript format is Claude Code's own and changes between versions. When a
  field you expect is missing, look at a few `entry` values before concluding anything.

**`cmux`: the terminal multiplexer's agent journal.**

| View | What |
|---|---|
| `cmux.journal` | `occurred_at`, `kind` (such as `agent.turn.started`, `agent.turn.completed`), `native_event` (the Claude Code hook: `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `Stop`, ...), `session_id`, `workspace_id` and `surface_id` (which cmux workspace and pane), `is_subagent` |

- **Joins:** `session_id` joins `claude`.
- **Turns:** a turn runs from `UserPromptSubmit` to `Stop`. That is when the agent worked; the
  gap until the next prompt is the developer's.
- **Content:** metadata only, no text.

**`hammerspoon`: what was on screen.**

| View | What |
|---|---|
| `hammerspoon.events` | `occurred_at`, `event`, `app`, `bundle_id`, `title` (window title), `space` (macOS Space), `idle_s` (seconds since the last input), `screen_active` |
| `hammerspoon.focus` | focus and title changes, each with `duration` until the machine's next event |

Events:
- **Focus:** `window_focused` and `window_title_changed`.
- **Spaces:** `space_changed`.
- **Away:** `screens_did_lock`, `screens_did_sleep`, `system_will_sleep` and the like.
- **Samples:** `sample`, every 30 s while there has been input in the last 5 minutes.

Titles tell a lot:
- **Browsers:** the page title.
- **Editors:** the file and the project.
- **Terminals:** whatever the shell or cmux sets.

Rows without a `title` are from before Hammerspoon had Accessibility. A long `duration` with a
large `idle_s` on the next sample is time away, not focus. DuckDB does not `sum()` an
`INTERVAL`: sum `epoch(duration)`, which is seconds.

**`git`: repositories in Claude Code's history.**

| View | What |
|---|---|
| `git.commits` | `occurred_at` (committer time), `repo`, `sha`, `parents`, `author_*`, `message`, `subject`, `claude` (the message has a `Co-Authored-By: Claude` trailer) |
| `git.ref_updates` | reflog entries: `ref`, `sha`, `message`. `update by push` on `refs/remotes/...` is a push from this machine; `fetch ...` is someone's push, fetched |
| `git.remotes` | each repository's remotes and URLs |

`repo` is the repository's top level. A Claude `cwd` or `project_path` is in a repository when
it starts with `repo`.

**The developer's own sources:** schemas not described here come from SQL files of the
developer's, listed in `ingest.migrations` as `config/<file>`. Read those files, in
`~/.config/observability-host/v1/explore/`: their comments say what the rows mean.

**`host`:** `host.heartbeats`, the agent's own pulse once a minute. A gap means the machine was
asleep or the agent was stopped. Use it to tell "no activity" from "no data".

## Joining

- **Session:** `claude.*.session_id` = `cmux.journal.session_id`.
- **Repository:** `starts_with(claude.prompts.project_path, git.commits.repo)`, or a Bash
  tool call's `cwd`.
- **Commit and session:** a commit made in a session has its `git commit` in that session's
  `tool_calls`, and its sha in the tool result. Otherwise match by repository and time.
- **Time:** what was on screen while an agent worked is the `hammerspoon.focus` intervals that
  overlap a cmux turn.

## Ways in

These are starting points; check each against the data before relying on it.

```sql
-- a timeline of the last hours, all sources
SELECT occurred_at, 'prompt' AS what, left(prompt, 120) AS detail FROM claude.prompts
  WHERE source = 'transcript' AND occurred_at > now() - INTERVAL 4 HOUR
UNION ALL SELECT occurred_at, 'commit', repo || ': ' || subject FROM git.commits WHERE occurred_at > now() - INTERVAL 4 HOUR
UNION ALL SELECT occurred_at, 'push', repo || ' ' || ref FROM git.ref_updates
  WHERE message = 'update by push' AND occurred_at > now() - INTERVAL 4 HOUR
UNION ALL SELECT occurred_at, 'focus', app || ': ' || coalesce(title, '') FROM hammerspoon.focus
  WHERE occurred_at > now() - INTERVAL 4 HOUR AND duration > INTERVAL 1 MINUTE
ORDER BY 1;

-- the sessions of the last hours, with their prompts
SELECT s.title, s.project, min(p.occurred_at) AS first, count(*) AS prompts
FROM claude.prompts p JOIN claude.sessions s USING (session_id)
WHERE p.source = 'transcript' AND p.occurred_at > now() - INTERVAL 4 HOUR
GROUP BY ALL ORDER BY first;

-- screen time per app, intervals over 30 minutes left out as time away
SELECT app, round(sum(epoch(duration)) / 60) AS minutes, count(*) AS switches FROM hammerspoon.focus
WHERE occurred_at > now() - INTERVAL 4 HOUR AND duration < INTERVAL 30 MINUTE
GROUP BY app ORDER BY minutes DESC;
```

## Reporting

- Answer what was asked first, then what else stands out.
- With each claim, give the query that supports it, so the developer can run it in the DuckDB
  UI.
- Separate what the data shows from what you infer. "No commits" might mean no data for that
  repository; check coverage first.
- **Content:** prompts and tool output can contain secrets that a tool printed, and other
  people's names and messages. Quote only what the answer needs, and never a credential or
  token.
