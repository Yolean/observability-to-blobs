# developer-feed/explore: the feed in DuckDB

`explore.sh` reads the bucket into a DuckDB database in the user-level state
(`~/.local/state/observability-host/v1/explore/feed.duckdb`). It ingests from `<since>` back
(default `24h`), then again every `--every` minutes (default 5). It serves the database with
quack, DuckDB's client-server protocol, on localhost only, for `y-duckdb -ui` or any other
client.

It uses `y-duckdb` (ystack, DuckDB 2.0.0).

## Credential

A read credential is minted with `dev-feed-r2 mint --role read --prefix dev1/` from a
read-only parent token, which the script says how to create, with a link to the account's R2
API tokens page. The parent's Access Key ID is kept in the config
(`DEV_FEED_READ_PARENT_KEY_ID`); its secret is pasted at the prompt and never kept. The
credential lasts `--ttl` (default `12h`). It goes to DuckDB as a temporary secret, on stdin,
never in a file or a command line. When it expires the script stops ingesting and keeps
serving.

## quack

`quack_serve('quack:localhost:<port>')` listens on `[::1]` only. The script checks the
listener after starting it, and stops if anything beyond loopback can reach the port. The
token is new every run, printed once, and not kept:

```sql
ATTACH 'quack:localhost:18338' AS feed (TOKEN '<printed token>');
FROM feed.claude.prompts ORDER BY occurred_at DESC;
```

## Ingest

- **Listing:** the script lists one prefix per machine and hour,
  `dev1/<cluster>/<date>/<hour>/`. The first ingest covers the hours since `<since>`; each
  later one covers the last three hours, as an object lands in its first line's hour and may
  arrive late. Objects older than that which arrive late, after a machine was offline, are
  picked up by the next start of the script.
- **Machines:** `--clusters` names them; the default is this machine's `CLUSTER_NAME`.
- **Storage:** each object's lines go into `ingest.lines` as JSON, and the object into
  `ingest.files`, in one transaction, so an object is never read twice nor half.

## Schemas

`sql/` holds the schemas: numbered, idempotent, applied in order at every start and recorded
in `ingest.migrations`. Everything after `001` is views over the raw lines, so a change of the
feed's line contract changes a view, not the stored data.

| Schema | Views |
|---|---|
| `ingest` | `files`, `lines`, `rows` (the columns every line has), `migrations` |
| `claude` | `lines`, `entries`, `prompts` (transcripts and history), `tool_calls`, `sessions`, `memory_versions`, `memories` |
| `git` | `remotes`, `ref_updates`, `commits` (with `claude`, whether Claude co-authored it) |
| `cmux` | `journal` |
| `host` | `heartbeats` |

## Tested

Run against a local versitygw with seeded objects of every row kind:
- the first ingest within `<since>`;
- a later object ingested alone by the next run;
- every view queried through quack from a second `y-duckdb`;
- the listener on `[::1]` only, and the LAN address refused.

Not run against R2.
