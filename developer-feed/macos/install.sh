#!/usr/bin/env bash
# Installs the user-level host agent with the developer feed's collectors on
# this Mac, from build.sh's target/macos/, no arguments
# (developer-feed/macos/README.md):
#
#   developer-feed/macos/install.sh
#
# The program is copied to OBSERVABILITY_HOST_PREFIX, never linked to the
# checkout. The config (developer-feed/lib/config.sh) gets every setting
# the agent reads, with what this script can tell (CLUSTER_NAME and
# NODE_NAME this Mac's LocalHostName: for the user-level agent each machine
# is a cluster of its own, under dev1/<host>/) for those not set; what is
# set stays. The LaunchAgent is (re)loaded when the config has a bucket and the
# Keychain a credential, and the script says what is missing when not.
# Run it again after build.sh to update.
#
# OBSERVABILITY_HOST_LAUNCHD=off installs and configures without touching
# ~/Library/LaunchAgents or launchd: for tests, which also point the
# OBSERVABILITY_HOST_* directories elsewhere.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
# shellcheck source=../lib/config.sh
. "$here/../lib/config.sh"
built=$repo/target/macos
host=$repo/images/fluentbit/host
prefix=$OBSERVABILITY_HOST_PREFIX
config=$OBSERVABILITY_HOST_CONFIG
state=$OBSERVABILITY_HOST_STATE
label=com.yolean.observability-host
plist=$HOME/Library/LaunchAgents/$label.plist
say() { echo "install: $*" >&2; }

[ -x "$built/bin/dev-feed-r2" ] && [ -x "$built/bin/dev-feed-git" ] && [ -x "$built/bin/dev-feed-backfill" ] \
  || { echo "install: nothing built in $built; run developer-feed/macos/build.sh" >&2; exit 2; }

echo "== program $prefix"
umask 022
mkdir -p "$prefix/bin" "$prefix/lib" "$prefix/etc"
if [ -x "$built/bin/fluent-bit" ]; then
  install -m 0755 "$built/bin/fluent-bit" "$prefix/bin/fluent-bit"
  rm -rf "$prefix/bin/fluent-bit.dSYM"
  [ ! -d "$built/bin/fluent-bit.dSYM" ] || cp -R "$built/bin/fluent-bit.dSYM" "$prefix/bin/"
  say "fluent-bit: $("$prefix/bin/fluent-bit" --version | head -1)"
else
  say "fluent-bit: not built yet; the tools are installed, the agent is not started"
fi
[ ! -f "$built/BUILD" ] || install -m 0644 "$built/BUILD" "$prefix/BUILD"
install -m 0755 "$host/launch.sh" "$prefix/bin/observability-host-launch"
install -m 0755 "$host/heartbeat.sh" "$prefix/bin/observability-host-heartbeat"
install -m 0755 "$built/bin/dev-feed-r2" "$built/bin/dev-feed-git" "$built/bin/dev-feed-backfill" "$prefix/bin/"
install -m 0755 "$repo/developer-feed/bin/dev-feed-cmux" "$repo/developer-feed/bin/dev-feed-snapshot" \
  "$here/dev-feed-agent" "$here/dev-feed-credentials" "$prefix/bin/"
install -m 0644 "$repo/developer-feed/lib/config.sh" "$prefix/lib/config.sh"
# the host's pipeline and collectors, and the feed's, replaced whole
rm -rf "$prefix/etc/host" "$prefix/etc/developer-feed"
mkdir -p "$prefix/etc/host/collectors.d" "$prefix/etc/host/parts.d"
install -m 0644 "$host/host.yaml" "$host/collectors.defaults" "$prefix/etc/host/"
install -m 0644 "$host"/collectors.d/*.yaml "$prefix/etc/host/collectors.d/"
install -m 0644 "$host"/parts.d/*.yaml "$prefix/etc/host/parts.d/"
cp -R "$repo/developer-feed/collectors" "$prefix/etc/developer-feed"
say "from commit $(sed -n 's/^commit=//p' "$prefix/BUILD" 2>/dev/null || echo unknown), uncommitted changes $(sed -n 's/^dirty=//p' "$prefix/BUILD" 2>/dev/null || echo unknown)"

echo "== config $config"
cfg_init
host=$(scutil --get LocalHostName 2>/dev/null || hostname -s)
cfg_default CLUSTER_NAME "$host"
cfg_default NODE_NAME "$host"
cfg_default KEY_ROOT dev1
cfg_default S3_REGION auto
cfg_default HOST_TLS_CA_FILE /etc/ssl/cert.pem
cfg_default HOST_LOG_LEVEL warn
# fluent-bit's node_exporter collectors that exist on darwin, for when the
# node collector is turned on in args
cfg_default HOST_NODE_COLLECTORS cpu,loadavg,meminfo,diskstats,filesystem,uname,stat,netdev
if [ -f "$config/args" ]; then
  say "$config/args (kept)"
else
  (umask 077 && cat >"$config/args" <<'ARGS'
# The collectors' flags (HOST_MODE.md, Collectors). The feed's collectors are
# on by default from their own directory; of the host's own, journal and files
# are Linux's, and node and self only serve a metrics port that nothing reads
# here. With them off the agent listens on no port.
--no-collector.journal
--no-collector.files
--no-collector.node
--no-collector.self
ARGS
)
  say "$config/args (created)"
fi
awsconfig="[default]
credential_process = $prefix/bin/dev-feed-credentials"
if [ "$(cat "$config/aws-config" 2>/dev/null)" = "$awsconfig" ]; then
  say "$config/aws-config (unchanged)"
else
  (umask 077 && printf '%s\n' "$awsconfig" >"$config/aws-config")
  say "$config/aws-config (written: credential_process, no secret)"
fi

echo "== state $state"
if [ -d "$state" ]; then
  say "$state (kept)"
else
  (umask 077 && mkdir -p "$state/log")
  say "$state (created)"
fi
mkdir -p "$state/log"

echo "== the agent"
missing=
for k in S3_BUCKET S3_ENDPOINT; do
  [ -n "$(cfg_get $k)" ] || missing="$missing $k (developer-feed/cloudflare/setup-bucket.sh);"
done
[ -x "$prefix/bin/fluent-bit" ] || missing="$missing fluent-bit (developer-feed/macos/build.sh);"
"$prefix/bin/dev-feed-credentials" expiry >/dev/null 2>&1 \
  || missing="$missing a credential ($prefix/bin/dev-feed-r2 mint | $prefix/bin/dev-feed-credentials store);"
if [ "${OBSERVABILITY_HOST_LAUNCHD:-on}" = off ]; then
  say "not started: OBSERVABILITY_HOST_LAUNCHD=off; would be missing:${missing:- nothing}"
  exit 0
fi
if [ -n "$missing" ]; then
  if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then say "$label is loaded from an earlier install; left as it is"; fi
  say "not started, missing:$missing"
  exit 0
fi
mkdir -p "$(dirname "$plist")"
cat >"$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>$prefix/bin/dev-feed-agent</string></array>
  <key>EnvironmentVariables</key><dict>
    <key>OBSERVABILITY_HOST_PREFIX</key><string>$prefix</string>
    <key>OBSERVABILITY_HOST_CONFIG</key><string>$config</string>
    <key>OBSERVABILITY_HOST_STATE</key><string>$state</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <!-- tail keeps every file it follows open, a transcript each, and
       launchd's default limit is 256 -->
  <key>SoftResourceLimits</key><dict><key>NumberOfFiles</key><integer>16384</integer></dict>
  <key>HardResourceLimits</key><dict><key>NumberOfFiles</key><integer>16384</integer></dict>
  <key>ProcessType</key><string>Background</string>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>StandardOutPath</key><string>$state/log/agent.log</string>
  <key>StandardErrorPath</key><string>$state/log/agent.log</string>
</dict>
</plist>
PLIST
chmod 0644 "$plist"
say "$plist (written)"
if launchctl bootout "gui/$(id -u)/$label" 2>/dev/null; then
  # bootout returns before the job is gone, and a bootstrap before then
  # fails with "Input/output error"
  for _ in $(seq 60); do
    launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1 || break
    sleep 0.5
  done
  say "$label stopped"
fi
launchctl bootstrap "gui/$(id -u)" "$plist"
say "$label started; log $state/log/agent.log"
