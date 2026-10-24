#!/usr/bin/env bash
# Stops the user-level host agent and removes its LaunchAgent
# (developer-feed/macos/README.md). The program, the config, the state and
# the Keychain item are kept unless --purge: the state holds the collectors'
# positions, without which a reinstall starts over as a first start.
#
#   developer-feed/macos/uninstall.sh [--purge]
set -euo pipefail
# shellcheck source=../lib/config.sh
. "$(cd "$(dirname "$0")/.." && pwd)/lib/config.sh"
label=com.yolean.observability-host
if launchctl bootout "gui/$(id -u)/$label" 2>/dev/null; then echo "uninstall: $label stopped"; else echo "uninstall: $label was not loaded"; fi
rm -f "$HOME/Library/LaunchAgents/$label.plist" && echo "uninstall: removed its plist"
if [ "${1:-}" = --purge ]; then
  rm -rf "$OBSERVABILITY_HOST_PREFIX" "$OBSERVABILITY_HOST_CONFIG" "$OBSERVABILITY_HOST_STATE"
  security delete-generic-password -s observability-host-v1 -a "$(id -un)" >/dev/null 2>&1 || true
  echo "uninstall: removed $OBSERVABILITY_HOST_PREFIX, $OBSERVABILITY_HOST_CONFIG, $OBSERVABILITY_HOST_STATE and the Keychain item"
fi
