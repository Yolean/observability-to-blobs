#!/usr/bin/env bash
# Runs heartbeat.sh against a temporary state directory and checks the
# counter, the row and the restart-flag metrics. No fluent-bit, no root.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
hb=$here/heartbeat.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/heartbeat-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "  PASS: $*"; }
check() { # check <description> <jq filter that must output true>
  [ "$(jq -r "$2" <<<"$row")" = true ] || fail "$1 (row: $row)"
  ok "$1"
}

export HEARTBEAT_STATE_DIR=$tmp/state
export HEARTBEAT_BOOT_ID_FILE=$tmp/boot_id
export HEARTBEAT_REBOOT_FLAG=$tmp/reboot-required
export NODE_NAME=testnode
export HOST_METRICS_PORT=2023
mkdir "$HEARTBEAT_STATE_DIR"
echo aaaaaaaa-1111-2222-3333-444444444444 >"$HEARTBEAT_BOOT_ID_FILE"

run() { row=$("$hb"); }

echo "counter"
run
check "the first run is a metrics row of scrape node for the node's instance" \
  '.table == "metrics" and .scrape == "node" and .instance == "testnode:2023"'
check "one sample, a counter named observability_host_heartbeat, value 1" \
  '(.samples | length) == 1 and .samples[0].name == "observability_host_heartbeat" and .samples[0].type == "counter" and .samples[0].value == 1'
check "without HOST_COLLECTORS the boot id is the sample's only label" \
  '.samples[0].labels == {"boot_id": "aaaaaaaa-1111-2222-3333-444444444444"}'
row=$(HOST_COLLECTORS=files,heartbeat,journal "$hb")
check "with HOST_COLLECTORS from the launcher, the running collectors as a label too" \
  '.samples[0].labels == {"boot_id": "aaaaaaaa-1111-2222-3333-444444444444", "collectors": "files,heartbeat,journal"}'
check "the row has no other key than raw.sql's" \
  '(keys - ["table","time","scrape","instance","samples"]) == []'
check "time is UTC to the nanosecond, as the node agent's scrapes have it" \
  '.time | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\\.[0-9]{9}Z$")'
run
check "the counter increments across runs of the same boot" '.samples[0].value == 3'
echo bbbbbbbb-1111-2222-3333-444444444444 >"$HEARTBEAT_BOOT_ID_FILE"
run
check "a new boot id starts the counter again, under the new boot id" \
  '.samples[0].value == 1 and .samples[0].labels.boot_id == "bbbbbbbb-1111-2222-3333-444444444444"'
run
check "and it continues from there" '.samples[0].value == 2'
echo "garbage" >"$HEARTBEAT_STATE_DIR/heartbeat-counter"
run
check "a damaged state file starts again at 1" '.samples[0].value == 1'
: >"$HEARTBEAT_STATE_DIR/heartbeat-counter"
run
check "an empty state file starts again at 1" '.samples[0].value == 1'
[ "$(cat "$HEARTBEAT_STATE_DIR/heartbeat-counter")" = "bbbbbbbb-1111-2222-3333-444444444444 1" ] \
  || fail "the state file holds the boot id and the counter"
ok "the state file holds the boot id and the counter"
chmod u-w "$HEARTBEAT_STATE_DIR"
if [ "$(id -u)" != 0 ]; then
  if "$hb" >/dev/null 2>&1; then fail "a state directory that cannot be written is an error, not a heartbeat"; fi
  ok "a state directory that cannot be written is an error, not a heartbeat"
fi
chmod u+w "$HEARTBEAT_STATE_DIR"

echo "restart flag"
prom=$HEARTBEAT_STATE_DIR/reboot-required.prom
run
grep -qx 'node_reboot_required 0' "$prom" || fail "node_reboot_required is 0 without a flag"
grep -qx 'node_reboot_required_since_seconds 0' "$prom" || fail "since is 0 without a flag"
grep -q 'node_reboot_required_packages' "$prom" && fail "no package series without a flag"
ok "no flag: 0, since 0, no packages"
TZ=UTC touch -t 202311142213.20 "$HEARTBEAT_REBOOT_FLAG"  # 1700000000, a form GNU and BSD touch both take
printf 'linux-base\nlibc6\nlinux-base\n\nweird"pkg\\name\n' >"$HEARTBEAT_REBOOT_FLAG.pkgs"
run
grep -qx 'node_reboot_required 1' "$prom" || fail "node_reboot_required is 1 with the flag"
grep -qx 'node_reboot_required_since_seconds 1700000000' "$prom" || fail "since is the flag's mtime"
grep -qx 'node_reboot_required_packages{package="linux-base"} 1' "$prom" || fail "a package series"
grep -qx 'node_reboot_required_packages{package="libc6"} 1' "$prom" || fail "another package series"
[ "$(grep -c '^node_reboot_required_packages{' "$prom")" = 3 ] || fail "one series per distinct package, 3 here (got: $(grep '^node_reboot_required_packages{' "$prom"))"
ok "flag: 1, since the flag's mtime, one series per distinct package"
grep -q '[\\"]' <(grep '^node_reboot_required_packages' "$prom" | sed 's/{package="//; s/"} 1$//') && fail "package names are reduced to safe characters"
ok "package names are reduced to characters that need no escaping"
# every line is exposition format: comment, or name{labels} value
grep -vE '^(#|[a-z_]+(\{[a-z_]+="[^"]*"\})? [0-9]+$)' "$prom" && fail "the file is Prometheus exposition format"
ok "the file is Prometheus exposition format"
[ -z "$(find "$HEARTBEAT_STATE_DIR" -name '*.prom*' ! -name reboot-required.prom)" ] || fail "no temporary file is left behind or ends in .prom"
ok "the write left no temporary file"
rm "$HEARTBEAT_REBOOT_FLAG" "$HEARTBEAT_REBOOT_FLAG.pkgs"
run
grep -qx 'node_reboot_required 0' "$prom" || fail "the flag removed: 0 again"
ok "the flag removed: 0 again"

echo "PASS: heartbeat"
