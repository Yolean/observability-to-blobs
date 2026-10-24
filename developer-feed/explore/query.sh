#!/usr/bin/env bash
# A query of the database a running explore.sh serves, for an agent or a
# script (ANALYZE.md): the SQL from the arguments, or stdin, with the
# database attached as feed and in use, so that views are <schema>.<view>,
# and times shown in this machine's time zone.
#
#   DEV_FEED_QUACK_TOKEN=<token> developer-feed/explore/query.sh "FROM claude.prompts LIMIT 5"
#
# The token is the one explore.sh printed, from the environment: it goes to
# DuckDB on stdin, never on a command line, so it stays out of an agent's
# transcript, which the feed ships.
#
# Reads only: a statement that does not start with SELECT, FROM, WITH,
# DESCRIBE, SHOW, SUMMARIZE, EXPLAIN, VALUES, PIVOT or UNPIVOT is refused.
# That keeps an agent from changing explore's database by mistake; it is
# no protection, as quack does not hold an attach to READ_ONLY.
#
# Env: DEV_FEED_QUACK_TOKEN (required), DEV_FEED_QUACK_PORT (default 18338)
set -euo pipefail
: "${DEV_FEED_QUACK_TOKEN:?DEV_FEED_QUACK_TOKEN: the token explore.sh printed}"
port=${DEV_FEED_QUACK_PORT:-18338}
if [ $# -gt 0 ]; then sql="$*"; else sql=$(cat); fi
command -v y-duckdb >/dev/null || { echo "query: needs y-duckdb (ystack/bin)" >&2; exit 2; }

# each statement's first word, comments left out
while IFS= read -r first; do
  case $(printf '%s' "$first" | tr '[:lower:]' '[:upper:]') in
    ''|SELECT|FROM|WITH|DESCRIBE|SHOW|SUMMARIZE|EXPLAIN|VALUES|PIVOT|UNPIVOT|'('*) ;;
    *) echo "query: reads only; refused a statement starting with $first" >&2; exit 2;;
  esac
done < <(printf '%s\n' "$sql" | sed 's/--.*$//' | tr '\n' ' ' | tr ';' '\n' | awk '{ print $1 }')

zone=$(readlink /etc/localtime 2>/dev/null | sed 's#.*/zoneinfo/##')
{
  printf "ATTACH 'quack:localhost:%s' AS feed (TOKEN '%s');\n" "$port" "${DEV_FEED_QUACK_TOKEN//\'/\'\'}"
  echo "USE feed;"
  [ -z "$zone" ] || printf "SET TimeZone = '%s';\n" "$zone"
  printf '%s\n' "$sql"
} | y-duckdb
