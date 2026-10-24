# shellcheck shell=sh
# The user-level host agent's directories and its config file, for the
# feed's scripts to source (developer-feed/macos/README.md, Layout). The
# machine-level agent keeps the same split under /opt, /etc and /var/lib
# (HOST_MODE.md); this is the unprivileged one, with a version in the config
# and state paths:
#
#   OBSERVABILITY_HOST_PREFIX  the program   (~/.local/opt/observability-host)
#   OBSERVABILITY_HOST_CONFIG  the config    (~/.config/observability-host/v1)
#   OBSERVABILITY_HOST_STATE   the state     (~/.local/state/observability-host/v1)
#
# The config dir holds env (KEY=VALUE lines the agent sources), args (the
# collectors' flags) and aws-config (credential_process only). No secret is
# ever written to any of them. Every non-secret a script produces goes into
# env with cfg_set, which says on stderr what it did.

OBSERVABILITY_HOST_PREFIX=${OBSERVABILITY_HOST_PREFIX:-$HOME/.local/opt/observability-host}
OBSERVABILITY_HOST_CONFIG=${OBSERVABILITY_HOST_CONFIG:-$HOME/.config/observability-host/v1}
OBSERVABILITY_HOST_STATE=${OBSERVABILITY_HOST_STATE:-$HOME/.local/state/observability-host/v1}
cfg_file=$OBSERVABILITY_HOST_CONFIG/env

cfg_say() { echo "config: $*" >&2; }

# cfg_init: the config dir and its env file, readable by the user only
cfg_init() {
  if [ ! -d "$OBSERVABILITY_HOST_CONFIG" ]; then
    (umask 077 && mkdir -p "$OBSERVABILITY_HOST_CONFIG")
    cfg_say "created $OBSERVABILITY_HOST_CONFIG"
  fi
  if [ ! -f "$cfg_file" ]; then
    (umask 077 && printf '%s\n' \
      "# The user-level host agent's settings (developer-feed/macos/README.md)." \
      "# Maintained by the setup scripts, which say what they change; edits stay." \
      "# No secrets: the bucket credential is in the login Keychain." >"$cfg_file")
    cfg_say "created $cfg_file"
  fi
}

# cfg_get KEY: the value in env, empty when unset
cfg_get() {
  [ -f "$cfg_file" ] || return 0
  sed -n "s/^$1=//p" "$cfg_file" | tail -n 1
}

# cfg_set KEY VALUE: sets KEY in env, and says whether it was added,
# changed (from what) or already so. Values are plain words: no spaces,
# quotes or $, so that the file is safe to source.
cfg_set() {
  key=$1 value=$2
  case $key in *[!A-Z0-9_]*|"") echo "cfg_set: bad key $key" >&2; return 2;; esac
  case $value in *[!A-Za-z0-9._:/@+,=-]*) echo "cfg_set: $key: value $value has characters env can't hold" >&2; return 2;; esac
  cfg_init
  old=$(cfg_get "$key")
  if grep -q "^$key=" "$cfg_file"; then
    if [ "$old" = "$value" ]; then
      cfg_say "$key=$value (unchanged)"
      return 0
    fi
    tmp=$cfg_file.tmp
    (umask 077 && awk -v k="$key" -v v="$value" 'BEGIN { FS = OFS = "=" } $1 == k { print k "=" v; next } { print }' "$cfg_file" >"$tmp")
    mv "$tmp" "$cfg_file"
    cfg_say "$key=$value (was $old)"
  else
    printf '%s=%s\n' "$key" "$value" >>"$cfg_file"
    cfg_say "$key=$value (added)"
  fi
}

# cfg_default KEY VALUE: sets KEY only when it is not set, so that an edit
# stays; says which
cfg_default() {
  if grep -q "^$1=" "$cfg_file" 2>/dev/null; then
    cfg_say "$1=$(cfg_get "$1") (kept)"
  else
    cfg_set "$1" "$2"
  fi
}
