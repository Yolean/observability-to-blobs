#!/bin/sh
# The host agent's heartbeat (HOST_MODE.md), run by host.yaml's exec input
# every 60 s. Each run:
#  - increments a counter kept in the state directory; the counter starts
#    again at 1 when the boot id changes and continues across restarts of
#    the service;
#  - prints one JSON line, a raw1 metrics row: the sample
#    observability_host_heartbeat{boot_id="..."} (a counter, the counter's
#    value) and the time, which the pipeline completes with cluster and node
#    and takes as the row's event time;
#  - writes the restart flag as textfile metrics (node_reboot_required,
#    node_reboot_required_since_seconds, node_reboot_required_packages) to
#    a .prom file in the state directory, for the node_exporter textfile
#    collector, by writing a temporary file and renaming it.
#
# The state is written before the line is printed: a crash in between
# skips a value, which shows as a gap, instead of repeating one.
#
# Environment (all optional; the overrides are for tests):
#   HEARTBEAT_STATE_DIR   /var/lib/observability-host
#   HEARTBEAT_BOOT_ID_FILE /proc/sys/kernel/random/boot_id
#   HEARTBEAT_REBOOT_FLAG /var/run/reboot-required (the package list is
#                         HEARTBEAT_REBOOT_PKGS, default <flag>.pkgs)
#   NODE_NAME, HOST_METRICS_PORT   the row's instance, node:port
set -eu

state_dir=${HEARTBEAT_STATE_DIR:-/var/lib/observability-host}
boot_id_file=${HEARTBEAT_BOOT_ID_FILE:-/proc/sys/kernel/random/boot_id}
flag=${HEARTBEAT_REBOOT_FLAG:-/var/run/reboot-required}
pkgs=${HEARTBEAT_REBOOT_PKGS:-$flag.pkgs}
counter_file=$state_dir/heartbeat-counter
prom_file=$state_dir/reboot-required.prom
instance=${NODE_NAME:-unknown}:${HOST_METRICS_PORT:-2023}

# a file in the state directory under its final name or not at all
write_atomic() { # write_atomic <path>: stdin becomes the file
  tmp=$(mktemp "$(dirname "$1")/.$(basename "$1").XXXXXX")
  if cat >"$tmp" && { sync "$tmp" 2>/dev/null || true; } && chmod 0644 "$tmp" && mv -f "$tmp" "$1"; then
    return 0
  fi
  rm -f "$tmp"
  return 1
}

# the value of a JSON string or a Prometheus label: no control characters,
# backslash and double quote escaped
quote() {
  printf '%s' "$1" | tr -d '\000-\037' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

boot_id=$(tr -d '[:space:]' <"$boot_id_file")
[ -n "$boot_id" ] || { echo "heartbeat: empty boot id in $boot_id_file" >&2; exit 1; }
boot_id=$(quote "$boot_id")

saved_boot=""
saved_n=""
if [ -r "$counter_file" ]; then
  read -r saved_boot saved_n <"$counter_file" || true
fi
case $saved_n in (''|*[!0-9]*) saved_boot=""; saved_n=0;; esac
if [ "$saved_boot" = "$boot_id" ]; then
  counter=$((saved_n + 1))
else
  counter=1
fi
printf '%s %s\n' "$boot_id" "$counter" | write_atomic "$counter_file" \
  || { echo "heartbeat: cannot write $counter_file" >&2; exit 1; }

# The restart flag. A failure here must not keep the heartbeat from being
# sent; the missing file is what the textfile collector then shows.
write_flag_metrics() {
  if [ -e "$flag" ]; then
    required=1
    since=$(stat -c %Y "$flag" 2>/dev/null || stat -f %m "$flag" 2>/dev/null || echo 0)
  else
    required=0
    since=0
  fi
  cat <<PROM
# HELP node_reboot_required 1 when the host needs a restart (the reboot-required flag exists), else 0.
# TYPE node_reboot_required gauge
node_reboot_required $required
# HELP node_reboot_required_since_seconds Modification time of the reboot-required flag, Unix time; 0 when there is none.
# TYPE node_reboot_required_since_seconds gauge
node_reboot_required_since_seconds $since
PROM
  if [ "$required" = 1 ] && [ -r "$pkgs" ]; then
    # package names are [a-z0-9.+-] and sometimes a version after a colon
    list=$(sed -e 's/[^A-Za-z0-9.+:_~-]//g' -e '/^$/d' "$pkgs" | sort -u)
    if [ -n "$list" ]; then
      echo '# HELP node_reboot_required_packages 1 for each package that asks for the restart.'
      echo '# TYPE node_reboot_required_packages gauge'
      printf '%s\n' "$list" | while IFS= read -r pkg; do
        printf 'node_reboot_required_packages{package="%s"} 1\n' "$(quote "$pkg")"
      done
    fi
  fi
}
write_flag_metrics | write_atomic "$prom_file" || echo "heartbeat: cannot write $prom_file" >&2

# the row's time, to the nanosecond as the node agent's scrapes carry it
printf '{"table":"metrics","time":"%s","scrape":"node","instance":"%s","samples":[{"name":"observability_host_heartbeat","type":"counter","labels":{"boot_id":"%s"},"value":%s}]}\n' \
  "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "$(quote "$instance")" "$boot_id" "$counter"
