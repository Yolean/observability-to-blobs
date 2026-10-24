#!/usr/bin/env bash
# Stops the developer feed's agent and removes its LaunchAgent
# (developer-feed/macos/README.md). The installed files, the state and the
# Keychain item are kept unless --purge: the state holds the collectors'
# positions, without which a reinstall starts over as a first start.
#
#   developer-feed/macos/uninstall.sh [--purge]
set -euo pipefail
prefix=${DEV_FEED_PREFIX:-$HOME/.dev-feed}
label=com.yolean.dev-feed
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
rm -f "$HOME/Library/LaunchAgents/$label.plist"
echo "stopped and removed $label"
if [ "${1:-}" = --purge ]; then
  rm -rf "$prefix"
  security delete-generic-password -s dev-feed-put -a "$(id -un)" >/dev/null 2>&1 || true
  echo "removed $prefix and the Keychain item dev-feed-put"
fi
