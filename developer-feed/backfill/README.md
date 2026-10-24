# developer-feed/backfill: the history before the collectors

`dev-feed-backfill` ships to the feed's bucket what the collectors did not, because it was
written before they ran. The collectors are live only (`../README.md`, Collectors).

```sh
dev-feed-backfill                            # what it would ship, per category; ships nothing
dev-feed-backfill --ship                     # ship it
dev-feed-backfill --category hammerspoon --ship
dev-feed-backfill --out /tmp/objects         # the objects, written there; records nothing
```

It reads the agent's settings (`CLUSTER_NAME`, `NODE_NAME`, `S3_*`, `KEY_ROOT`) from the
environment or the config, and writes as the agent does: the agent's own credential (the
config's `credential_process`, or `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`), PutObject
only. `dev-feed-git` and `dev-feed-cmux` must be next to it or on `PATH`, and `sqlite3` on
`PATH`.

## Categories

| Category | Its collector | Reads | Time of a line |
|---|---|---|---|
| `claude-transcript` | `claude` | `~/.claude/projects/*/*.jsonl` | its `timestamp`; a line without one, the nearest earlier line's |
| `claude-subagent` | `claude` | `.../*/subagents/*.jsonl` | the same |
| `claude-history` | `claude` | `~/.claude/history.jsonl` | its `timestamp`, epoch ms |
| `claude-memory` | `claude-memory` | `~/.claude/projects/*/memory/*.md` | the file's modification time |
| `hammerspoon` | `hammerspoon` | the activity log | its `ts` |
| `cmux` | `cmux` | the agent journal, through `dev-feed-cmux rows` | `occurred_at` |
| `git` | `git` | reflogs and commits, through `dev-feed-git backfill` | the reflog entry's time, the committer time; a remote the run's |

**Rows:** each category's are its collector's, with `ts` the source's own time and `backfill`
the run (`../sql/feed.sql`). For the tails the backfill makes the rows the way the collector's
processors do, and the feed's e2e (`../itest/run.sh`) checks that a line shipped by both gets
the same fields. For `git` and `cmux` the collector's own command prints them.

**Objects:** they go under the hour of their first line, like the agent's, with 8 characters
of the content's hash where the agent has 8 random ones. Within an hour the lines are in the
order of their time, at most 32 MB uncompressed per object. The keys of each shipped run are
listed in `backfill/run-<id>.keys` in the agent's state, for redacting a run later.

## Where it stops

Each category keeps its own state in `backfill/` in the agent's state
(`~/.local/state/observability-host/v1/`). A run ships a category from where the previous run
stopped:

- **The collector has run:** up to where it is now, once, and then the category is handed
  off. Later runs leave it alone.
- **It has not:** up to now, and the next run goes on from there.

So a category that is new to the backfill, or to the feed, is shipped from its beginning by
the next run, whatever earlier runs shipped of the others. The state is saved only after every
object is uploaded. A run that fails ships its lines again, never loses one.

How far "up to where the collector is" reaches, per category:

| Category | The collector began | The backfill ships up to |
|---|---|---|
| tails | at a file's end if it found the file at an agent start, from its first line if later (fluent-bit's `read_from_head` off, `read_newly_discovered_files_from_head` on) | a file found at a start: the collector's offset now, read from a copy of its position database; a file found later: nothing |
| `cmux` | after the journal's newest sequence at its first run | the collector's sequence now |
| `git` | after each reflog's length at its first run | the reflogs' ends now, and the commits those entries brought |
| `claude-memory` | its first run ships every memory | nothing, once the collector has run |

The collectors keep only where they are now, not where they began. So the lines from where a
collector began to where it is now are shipped twice, once by each. Readers drop the second by
the line's identity, what the source wrote: explore's `ingest.rows`
(`../explore/sql/001-ingest.sql`). The same happens when a collector is stopped between
printing a line and saving its position.

**Agent starts:** `dev-feed-agent` appends each start's time to `agent-starts` in the state.
A file that a tail database registered within 15 s after a start is one the collector found
at that start. The earliest registration in any tail database also counts as a start, the
agent's first. A file that appeared within 15 s after a start is shipped by both.

## Tests

`go test ./...` covers the hand-off cases against a fake bucket. `../itest/run.sh` runs the
backfill after the agent, over the same fixtures, in two runs: the claude categories first,
then the rest. It checks:

- the lines written before the start arrive once, by the run of their category;
- no line is shipped twice by the backfill;
- a line shipped by both has the same fields;
- explore's `ingest.rows` has each line once.

`sigv4.go` and `sigv4_test.go` are copies of `compactor/`'s; `go generate` refreshes them.
