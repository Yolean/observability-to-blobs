#!/bin/sh
# Checks that a directory holds Claude Code sessions, in the layout that
# dev-feed-backfill --home reads (README.md, Sessions from a backup):
#
#   check-claude-sessions.sh <dir>
#
# finds every .jsonl under <dir> and checks each one under
# <home>/.claude/projects/<project>/:
#   - <session>.jsonl, a session's transcript, its name a session id;
#   - <session>/subagents/<agent>.jsonl, a subagent's.
# A transcript must be JSON lines, each an object with a type; its user and
# assistant entries must each have a uuid, a timestamp and the Claude Code
# version that wrote it; and it must name the session, as its sessionIds,
# and no other. A transcript without a conversation passes, with a note:
# Claude Code leaves a few lines of title and mode for a session renamed or
# left before anything happened. It prints a line per transcript, and then the --home to
# backfill them with. Exit status 1 if any check failed, or none was found.
# Needs jq (in /usr/bin on macOS).
set -eu

[ $# -eq 1 ] && [ -d "$1" ] || { echo "usage: check-claude-sessions.sh <dir>" >&2; exit 2; }
command -v jq >/dev/null || { echo "check-claude-sessions: needs jq" >&2; exit 2; }
dir=$(cd "$1" && pwd)
uuid='^[0-9a-f]\{8\}-[0-9a-f]\{4\}-[0-9a-f]\{4\}-[0-9a-f]\{4\}-[0-9a-f]\{12\}$'
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
find "$dir" -type f -name '*.jsonl' | sort >"$tmp/files"
: >"$tmp/homes"
failed=0 found=0

# a transcript's summary, in one pass; jq fails on a line that is not JSON
# shellcheck disable=SC2016 # jq's variables, not the shell's
summary='reduce inputs as $e ({lines: 0, bad: 0, types: {}, ids: {}, versions: {}, first: null, last: null, cwd: null, title: null};
  .lines += 1
  | if ($e | type) != "object" or ($e.type | type) != "string" then .bad += 1 else
      .types[$e.type] += 1
      | (if $e.sessionId then .ids[$e.sessionId] = true else . end)
      | (if $e.type == "user" or $e.type == "assistant" then
           (if ($e.uuid | type) != "string" or ($e.timestamp | type) != "string" or ($e.version | type) != "string"
            then .bad += 1 else .versions[$e.version] = true end)
           | .cwd //= $e.cwd
         else . end)
      | (if ($e.timestamp | type) == "string" then
           .first = ([.first, $e.timestamp] | map(select(.)) | min)
           | .last = ([.last, $e.timestamp] | map(select(.)) | max)
         else . end)
      | (if ($e.aiTitle | type) == "string" then .title = $e.aiTitle else . end)
    end)'

while IFS= read -r f; do
  rel=${f#*/.claude/projects/}
  if [ "$rel" = "$f" ]; then
    case $f in
      */.claude/history.jsonl)
        echo "skip  $f: the prompt history, which --category ...,claude-history ships"
        continue ;;
      */.claude/*)
        echo "skip  $f: Claude Code's, but no transcript"
        continue ;;
    esac
    echo "FAIL  $f: not under <home>/.claude/projects/<project>/, where --home reads"
    failed=$((failed + 1))
    continue
  fi
  home=${f%%/.claude/projects/*}
  # the path's parts under projects/: <project>/<session>.jsonl, or
  # <project>/<session>/subagents/<agent>.jsonl
  parts=$(printf '%s\n' "$rel" | tr '/' '\n' | wc -l | tr -d ' ')
  case $parts/$rel in
    2/*) kind=session id=$(basename "$f" .jsonl) ;;
    4/*/*/subagents/*) kind=subagent id=$(printf '%s\n' "$rel" | cut -d/ -f2) ;;
    *) echo "skip  $f: not a session's or a subagent's transcript, which the backfill does not read"; continue ;;
  esac
  project=$(printf '%s\n' "$rel" | cut -d/ -f1)
  if ! printf '%s\n' "$id" | grep -q "$uuid"; then
    echo "FAIL  $f: $id is not a session id"
    failed=$((failed + 1))
    continue
  fi
  if ! s=$(jq -n -c "$summary" "$f" 2>"$tmp/err"); then
    echo "FAIL  $f: not JSON lines: $(head -c 200 "$tmp/err")"
    failed=$((failed + 1))
    continue
  fi
  problems=$(printf '%s\n' "$s" | jq -r --arg id "$id" '[
      (select(.lines == 0) | "empty"),
      (select(.bad > 0) | "\(.bad) lines not Claude Code entries"),
      (select((.ids | keys) != [$id]) | "session ids \(.ids | keys | join(",")), not only \($id)")
    ] | join("; ")')
  if [ -n "$problems" ]; then
    echo "FAIL  $f: $problems"
    failed=$((failed + 1))
    continue
  fi
  found=$((found + 1))
  printf '%s\n' "$home" >>"$tmp/homes"
  printf '%s\n' "$s" | jq -r --arg kind "$kind" --arg id "$id" --arg project "$project" '
    "ok    \($kind) \($id) in \($project)" +
    (if (.types.user // 0) == 0 or (.types.assistant // 0) == 0
     then ", no conversation: \(.types | to_entries | map("\(.value) \(.key)") | join(", "))" else "" end) + "\n" +
    "      \(.first // "no time") to \(.last // "no time"), \(.lines) lines: \(.types.user // 0) user, \(.types.assistant // 0) assistant\n" +
    "      Claude Code \(.versions | keys | join(", ") | if . == "" then "?" else . end), cwd \(.cwd // "?")" +
    (if .title then "\n      \"\(.title)\"" else "" end)'
done <"$tmp/files"

echo
if [ "$found" -eq 0 ]; then
  echo "no Claude Code transcripts found under $dir"
  exit 1
fi
echo "$found transcripts, $failed failed"
[ "$failed" -eq 0 ] || exit 1
echo "backfill them, from this machine's install:"
sort -u "$tmp/homes" | while IFS= read -r h; do
  echo "  ~/.local/opt/observability-host/bin/dev-feed-backfill --home '$h'            # what it would ship"
  echo "  ~/.local/opt/observability-host/bin/dev-feed-backfill --home '$h' --ship"
done
