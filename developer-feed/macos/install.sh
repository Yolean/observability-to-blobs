#!/usr/bin/env bash
# Installs the developer feed's agent for the current user on macOS
# (developer-feed/macos/README.md): copies, never links, so that the agent
# runs what was installed and not whatever the checkout holds later. Run it
# again to update; the env and args files and the state are kept.
#
#   developer-feed/macos/install.sh [--fluent-bit <binary>] [--no-launchd]
#
#   --fluent-bit   the binary to install (default target/macos/fluent-bit,
#                  from build-fluent-bit.sh)
#   --no-launchd   install the files only; do not write or (re)load the
#                  LaunchAgent
#
# Env:
#   DEV_FEED_PREFIX   where it goes (default ~/.dev-feed)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
prefix=${DEV_FEED_PREFIX:-$HOME/.dev-feed}
fluent_bit=$repo/target/macos/fluent-bit
launchd=true
while [ $# -gt 0 ]; do
  case $1 in
    --fluent-bit) fluent_bit=$2; shift 2;;
    --no-launchd) launchd=false; shift;;
    *) echo "install.sh: unknown argument $1" >&2; exit 2;;
  esac
done
[ -x "$fluent_bit" ] || { echo "install.sh: no fluent-bit at $fluent_bit (build-fluent-bit.sh builds it)" >&2; exit 2; }
label=com.yolean.dev-feed
plist=$HOME/Library/LaunchAgents/$label.plist
host=$repo/images/fluentbit/host

umask 077
mkdir -p "$prefix"/{opt/bin,opt/etc,developer-feed/bin,state,log}

# the host agent: binary, launcher, heartbeat, pipeline and its collectors
install -m 0755 "$fluent_bit" "$prefix/opt/bin/fluent-bit"
install -m 0755 "$host/launch.sh" "$prefix/opt/bin/observability-host-launch"
install -m 0755 "$host/heartbeat.sh" "$prefix/opt/bin/observability-host-heartbeat"
rm -rf "$prefix/opt/etc/collectors.d" "$prefix/opt/etc/parts.d"
mkdir -p "$prefix/opt/etc/collectors.d" "$prefix/opt/etc/parts.d"
install -m 0644 "$host/host.yaml" "$host/collectors.defaults" "$prefix/opt/etc/"
install -m 0644 "$host"/collectors.d/*.yaml "$prefix/opt/etc/collectors.d/"
install -m 0644 "$host"/parts.d/*.yaml "$prefix/opt/etc/parts.d/"

# the feed: its collectors, their commands, the agent and the credential
rm -rf "$prefix/developer-feed/collectors"
cp -R "$repo/developer-feed/collectors" "$prefix/developer-feed/collectors"
(cd "$repo/developer-feed/git" && go build -trimpath -o "$prefix/developer-feed/bin/dev-feed-git" .)
install -m 0755 "$repo/developer-feed/bin/dev-feed-cmux" "$here/dev-feed-agent" "$here/dev-feed-credentials" "$prefix/developer-feed/bin/"

# settings: written once, kept on update
if [ ! -f "$prefix/env" ]; then
  cat >"$prefix/env" <<'ENV'
# The developer feed's settings (developer-feed/macos/README.md). No secrets
# here: the bucket credential is in the login Keychain (dev-feed-credentials).
# The feed, the same on all of this developer's machines; required.
CLUSTER_NAME=
# The bucket and its S3 endpoint; required. R2:
# https://<account id>[.eu].r2.cloudflarestorage.com
S3_BUCKET=
S3_ENDPOINT=
# NODE_NAME=       default: this Mac's LocalHostName
# KEY_ROOT=dev1
# HOST_LOG_LEVEL=warn
# DEV_FEED_GIT_ROOTS=     more repositories for the git collector, colon-separated globs
ENV
fi
if [ ! -f "$prefix/args" ]; then
  cat >"$prefix/args" <<'ARGS'
# The collectors (HOST_MODE.md, Collectors): the feed's are on by default
# from developer-feed/collectors; of the host's own, journal and files are
# Linux's, and node only serves the metrics port, which nothing reads here.
--no-collector.journal
--no-collector.files
--no-collector.node
ARGS
fi
printf '[default]\ncredential_process = %s/developer-feed/bin/dev-feed-credentials\n' "$prefix" >"$prefix/aws-config"


echo "installed in $prefix"
"$prefix/opt/bin/fluent-bit" --version | head -1

$launchd || exit 0
# shellcheck disable=SC1091
if ! (set -a; . "$prefix/env"; [ -n "${CLUSTER_NAME:-}" ] && [ -n "${S3_BUCKET:-}" ] && [ -n "${S3_ENDPOINT:-}" ]); then
  echo "not started: set CLUSTER_NAME, S3_BUCKET and S3_ENDPOINT in $prefix/env, store the credential, run install.sh again"
  exit 0
fi
"$prefix/developer-feed/bin/dev-feed-credentials" expiry >/dev/null 2>&1 \
  || { echo "not started: no credential in the Keychain (developer-feed/macos/README.md, Credential)"; exit 0; }
mkdir -p "$(dirname "$plist")"
cat >"$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>$prefix/developer-feed/bin/dev-feed-agent</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>ProcessType</key><string>Background</string>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>StandardOutPath</key><string>$prefix/log/agent.log</string>
  <key>StandardErrorPath</key><string>$prefix/log/agent.log</string>
</dict>
</plist>
PLIST
chmod 0644 "$plist"
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"
echo "started: launchctl print gui/$(id -u)/$label; log in $prefix/log/agent.log"
