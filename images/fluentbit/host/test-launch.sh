#!/usr/bin/env bash
# Runs launch.sh against this directory's host.yaml, collectors.d/, parts.d/
# and collectors.defaults, with a stub in place of fluent-bit, and checks
# which collectors each set of flags runs and what configuration it writes.
# No fluent-bit, no root.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/launch-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "  PASS: $*"; }

export HOST_ETC=$here
export HOST_ARGS_FILE=$tmp/args
export STATE_DIRECTORY=$tmp/state
export HOST_FLUENT_BIT=$tmp/fluent-bit
# the stub's own $@ and variable, expanded when it runs
# shellcheck disable=SC2016
printf '#!/bin/sh\necho "$@" >"%s/fluent-bit.args"\necho "$HOST_COLLECTORS" >"%s/fluent-bit.collectors"\n' "$tmp" "$tmp" >"$HOST_FLUENT_BIT"
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
