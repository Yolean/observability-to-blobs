#!/usr/bin/env bash
# Runs launch.sh against this directory's host.yaml, collectors.d/, parts.d/
# and collectors.defaults, with a stub in place of fluent-bit, and checks
# which collectors each set of flags runs and what configuration it writes.
# No fluent-bit, no root.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/launch-test.XXXXXX")" && pwd)
trap 'rm -rf "$tmp"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "  PASS: $*"; }

export HOST_ETC=$here
export HOST_ARGS_FILE=$tmp/args
export STATE_DIRECTORY=$tmp/state
export HOST_FLUENT_BIT=$tmp/fluent-bit
# the stub's own $@ and variables, expanded when it runs
# shellcheck disable=SC2016
printf '#!/bin/sh\necho "$@" >"%s/fluent-bit.args"\necho "$HOST_STATE_DIR $HEARTBEAT_STATE_DIR" >"%s/fluent-bit.env"\necho "$HOST_COLLECTORS" >"%s/fluent-bit.collectors"\n' "$tmp" "$tmp" "$tmp" >"$HOST_FLUENT_BIT"
chmod +x "$HOST_FLUENT_BIT"

# enabled <flag ...>: the collectors the launcher would run, space-separated
enabled() { "$here/launch.sh" --print "$@" | sed -n 's/^# collectors: *//p'; }
expect() { # expect <description> <collectors> <flag ...>
  local want=$2 desc=$1 got
  shift 2
  got=$(enabled "$@") || fail "$desc: the launcher failed"
  [ "$got" = "$want" ] || fail "$desc: ran '$got', want '$want'"
  ok "$desc"
}
refuses() { # refuses <description> <message part> <flag ...>
  local desc=$1 part=$2 out
  shift 2
  if out=$("$here/launch.sh" --print "$@" 2>&1); then fail "$desc: accepted"; fi
  case $out in *"$part"*) ok "$desc";; *) fail "$desc: said '$out'";; esac
}

echo "flags"
expect "no flags and no args file: the defaults" "files heartbeat journal node self"
expect "--no-collector.journal leaves the journal out" "files heartbeat node self" --no-collector.journal
expect "--collector.disable-defaults leaves the forced heartbeat only" "heartbeat" --collector.disable-defaults
expect "disable-defaults with --collector.files" "files heartbeat" --collector.disable-defaults --collector.files
expect "disable-defaults applies wherever it is in the flags" "files heartbeat" --collector.files --collector.disable-defaults
expect "for one collector the last flag wins" "files heartbeat journal self" --no-collector.node --collector.node --no-collector.node
expect "--collector.heartbeat is accepted and changes nothing" "files heartbeat journal node self" --collector.heartbeat
refuses "--no-collector.heartbeat is refused" "heartbeat collector cannot be turned off" --no-collector.heartbeat
refuses "an unknown collector is refused, naming the ones there are" "no collector bogus; there are: files heartbeat journal node self" --collector.bogus
refuses "an unknown flag is refused" "unknown flag --collectors.files" --collectors.files

echo "args file"
printf '# a comment\n\n--no-collector.journal  --no-collector.node # trailing comment\n' >"$HOST_ARGS_FILE"
expect "its flags apply, comments and blank lines aside" "files heartbeat self"
expect "the command line comes after it" "files heartbeat node self" --collector.node
printf -- '--no-collector.heartbeat\n' >"$HOST_ARGS_FILE"
refuses "its flags are checked as the command line's" "cannot be turned off"
rm "$HOST_ARGS_FILE"

echo "configuration"
cfg=$("$here/launch.sh" --print)
grep -qx "  - $here/host.yaml" <<<"$cfg" || fail "host.yaml is not included"
ok "host.yaml, the service and the S3 output, is always included"
[ "$(grep -c "^  - $here/" <<<"$cfg")" = 7 ] || fail "the defaults include $(grep -c "^  - " <<<"$cfg") files, want 7: $cfg"
grep -qx "  - $here/parts.d/serve-metrics.yaml" <<<"$cfg" || fail "the defaults lack the metrics port"
ok "the defaults include host.yaml, the metrics port and the five collectors"
# the inputs and outputs of the defaults are those of the single host.yaml
# before the split: the aliases of every included file
aliases=$(sed -n 's/^  - //p' <<<"$cfg" | xargs sed -n 's/^ *alias: *//p' | sort | tr '\n' ' ')
[ "$aliases" = "blob files heartbeat journal node_exporter self_metrics " ] || fail "aliases: $aliases"
ok "the defaults' inputs and outputs: journal, files, heartbeat, node_exporter, self_metrics, blob"
grep -q 'name: prometheus_exporter' "$here/parts.d/serve-metrics.yaml" || fail "the part has no prometheus_exporter"
cfg=$("$here/launch.sh" --print --no-collector.node --no-collector.self)
! grep -q serve-metrics <<<"$cfg" || fail "the metrics port is included without node and self: $cfg"
ok "without node and self, no metrics port: nothing listens"
cfg=$("$here/launch.sh" --print --collector.disable-defaults --collector.self)
grep -q serve-metrics <<<"$cfg" || fail "self alone lacks the metrics port"
ok "self alone brings the metrics port"

echo "run"
"$here/launch.sh" --no-collector.journal -- --dry-run 2>"$tmp/stderr"
[ -f "$STATE_DIRECTORY/fluent-bit.yaml" ] || fail "no configuration in the state directory"
grep -qx "# collectors: files heartbeat node self" "$STATE_DIRECTORY/fluent-bit.yaml" || fail "the written configuration: $(cat "$STATE_DIRECTORY/fluent-bit.yaml")"
ok "the configuration is written to the state directory"
[ "$(cat "$tmp/fluent-bit.args")" = "-c $STATE_DIRECTORY/fluent-bit.yaml --dry-run" ] || fail "fluent-bit got: $(cat "$tmp/fluent-bit.args")"
ok "fluent-bit runs with it, and with the arguments after --"
grep -q "collectors: files heartbeat node self" "$tmp/stderr" || fail "stderr: $(cat "$tmp/stderr")"
ok "the collectors are logged"
[ "$(cat "$tmp/fluent-bit.collectors")" = "files,heartbeat,node,self" ] || fail "HOST_COLLECTORS: $(cat "$tmp/fluent-bit.collectors")"
ok "fluent-bit, and so the heartbeat, gets them in HOST_COLLECTORS"
[ ! -e "$STATE_DIRECTORY/fluent-bit.yaml.tmp" ] || fail "the temporary file was left"
ok "no temporary file is left"
[ "$(cat "$tmp/fluent-bit.env")" = "$STATE_DIRECTORY $STATE_DIRECTORY" ] || fail "fluent-bit's env: $(cat "$tmp/fluent-bit.env")"
ok "fluent-bit gets the state directory as HOST_STATE_DIR and HEARTBEAT_STATE_DIR"
HEARTBEAT_STATE_DIR=$tmp/hb "$here/launch.sh" 2>/dev/null
[ "$(cat "$tmp/fluent-bit.env")" = "$STATE_DIRECTORY $tmp/hb" ] || fail "fluent-bit's env: $(cat "$tmp/fluent-bit.env")"
ok "a HEARTBEAT_STATE_DIR that is set is kept"
for f in "$here/host.yaml" "$here"/collectors.d/*.yaml; do
  if grep -v '^ *#' "$f" | grep -q '/var/lib/observability-host'; then fail "$f names the state directory instead of HOST_STATE_DIR"; fi
done
ok "no collector and not host.yaml names the state directory: they use HOST_STATE_DIR"

echo "more directories of collectors"
extra=$tmp/extra
mkdir -p "$extra/collectors.d" "$extra/parts.d"
printf '# needs: extra-part serve-metrics
pipeline:
  inputs:
    - name: dummy
      alias: extra
' >"$extra/collectors.d/extra.yaml"
printf 'pipeline:
  outputs: []
' >"$extra/parts.d/extra-part.yaml"
printf 'pipeline:
  inputs:
    - name: dummy
      alias: optional
' >"$extra/collectors.d/optional.yaml"
printf 'extra
' >"$extra/collectors.defaults"
export HOST_COLLECTOR_DIRS=$extra
expect "a directory's defaults add to HOST_ETC's; its other collectors are off" "extra files heartbeat journal node self"
expect "its collectors take the flags" "files heartbeat optional" --collector.disable-defaults --collector.files --collector.optional
cfg=$("$here/launch.sh" --print --collector.disable-defaults --collector.extra)
grep -qx "  - $extra/collectors.d/extra.yaml" <<<"$cfg" || fail "the extra collector's file: $cfg"
grep -qx "  - $extra/parts.d/extra-part.yaml" <<<"$cfg" || fail "the extra part: $cfg"
grep -qx "  - $here/parts.d/serve-metrics.yaml" <<<"$cfg" || fail "a part from HOST_ETC: $cfg"
ok "the configuration includes its collector, its part, and HOST_ETC's part it needs"
printf 'pipeline:
  inputs: []
' >"$extra/collectors.d/files.yaml"
refuses "a collector name in two directories is refused" "collectors in more than one directory: files" --collector.files
rm "$extra/collectors.d/files.yaml"
HOST_COLLECTOR_DIRS=$tmp/nothing refuses "a directory without collectors.d/ is refused" "has no collectors.d/"
unset HOST_COLLECTOR_DIRS

echo "the machine's own collectors, beside the args file"
own=$(dirname "$HOST_ARGS_FILE")/collectors
mkdir -p "$own/collectors.d"
expect "an own collectors/ without collectors.defaults adds nothing by default" "files heartbeat journal node self"
printf 'pipeline:
  inputs:
    - name: dummy
      alias: mine
' >"$own/collectors.d/mine.yaml"
echo mine >"$own/collectors.defaults"
expect "its defaults add to the host's" "files heartbeat journal mine node self"
expect "its collectors take the flags" "heartbeat" --no-collector.mine --collector.disable-defaults
HOST_COLLECTOR_DIRS=$own expect "named in HOST_COLLECTOR_DIRS as well, it counts once" "files heartbeat journal mine node self"
rm -r "$own"

echo "list"
list=$("$here/launch.sh" --list --no-collector.journal)
grep -qx "heartbeat forced" <<<"$list" || fail "list: $list"
grep -qx "journal off, on by default" <<<"$list" || fail "list: $list"
grep -qx "node on, on by default" <<<"$list" || fail "list: $list"
ok "--list says each collector's state and whether it is a default"

echo "required"
printf '# the security log\nfiles journal\n' >"$tmp/required"
expect "a required collector runs with --collector.disable-defaults" "files heartbeat journal" --collector.disable-defaults
refuses "--no-collector of a required one is refused, naming the file" "the files collector is required on this host ($tmp/required)" --no-collector.files
grep -qx "journal required" <<<"$("$here/launch.sh" --list)" || fail "list: $("$here/launch.sh" --list)"
ok "--list says it is required"
echo nosuch >>"$tmp/required"
refuses "a required name that is no collector is refused" "requires nosuch, which has no collectors.d/nosuch.yaml"
HOST_REQUIRED_FILE=$tmp/elsewhere expect "HOST_REQUIRED_FILE names another file; a missing one requires none" "heartbeat" --collector.disable-defaults
rm "$tmp/required"

echo "all launch tests passed"
