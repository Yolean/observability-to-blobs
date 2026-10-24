#!/bin/sh
# The host agent's launcher (HOST_MODE.md, Collectors): picks the collectors
# to run, writes the fluent-bit configuration that includes them, and execs
# fluent-bit with it. The systemd unit runs it as
# /opt/observability-host/bin/observability-host-launch.
#
#   observability-host-launch [flag ...] [-- fluent-bit arg ...]
#
# Flags, as node_exporter's (README.md there, "Collectors"):
#   --collector.<name>             enable a collector
#   --no-collector.<name>          disable one that is on by default
#   --collector.disable-defaults   disable every collector not enabled
#                                  explicitly; forced ones stay on
#   --list                         print each collector and whether it runs,
#                                  then exit
#   --print                        print the configuration instead of running
# The flags in the args file come first, one or more per line, # for
# comments; the command line's come after them, and for one collector the
# last flag wins.
#
# Env:
#   HOST_ARGS_FILE   the args file (default /etc/observability-host/args;
#                    a missing file is no flags)
#   HOST_ETC         host.yaml, collectors.defaults, collectors.d/, parts.d/
#                    (default: etc/ beside this script's bin/)
#   STATE_DIRECTORY  where the configuration is written (systemd sets it from
#                    StateDirectory=; default /var/lib/observability-host)
#   HOST_FLUENT_BIT  the binary (default: fluent-bit beside this script)
set -eu

bin=$(cd "$(dirname "$0")" && pwd)
etc=${HOST_ETC:-$(cd "$bin/../etc" 2>/dev/null && pwd || echo "$bin/../etc")}
state=${STATE_DIRECTORY:-/var/lib/observability-host}
args_file=${HOST_ARGS_FILE:-/etc/observability-host/args}
fluent_bit=${HOST_FLUENT_BIT:-$bin/fluent-bit}

die() { echo "observability-host-launch: $*" >&2; exit 2; }

[ -f "$etc/host.yaml" ] || die "no host.yaml in $etc (HOST_ETC)"
[ -f "$etc/collectors.defaults" ] || die "no collectors.defaults in $etc (HOST_ETC)"

# every collector there is: collectors.d/<name>.yaml
available=$(cd "$etc/collectors.d" && for f in *.yaml; do [ -e "$f" ] && echo "${f%.yaml}"; done | sort)
defaults=$(sed -e 's/#.*//' "$etc/collectors.defaults" | awk 'NF { print $1 }')
forced=$(sed -e 's/#.*//' "$etc/collectors.defaults" | awk 'NF && $2 == "forced" { print $1 }')
for c in $defaults; do
  echo "$available" | grep -qx "$c" || die "collectors.defaults names $c, which has no collectors.d/$c.yaml"
done

# the flags: the args file's, then the command line's; the fluent-bit args
# after "--" are kept in "$@"
flags=
if [ -f "$args_file" ]; then
  flags=$(sed -e 's/#.*//' "$args_file" | tr -s ' \t' '\n' | sed '/^$/d')
fi
while [ $# -gt 0 ]; do
  case $1 in
    --) shift; break;;
    *) flags="$flags
$1"; shift;;
  esac
done

# explicit: one "<name> on|off" line per collector, the last flag's
explicit=
disable_defaults=false
mode=run
for f in $flags; do
  case $f in
    --collector.disable-defaults) disable_defaults=true;;
    --collector.*) explicit="$explicit
${f#--collector.} on";;
    --no-collector.*) explicit="$explicit
${f#--no-collector.} off";;
    --list) mode=list;;
    --print) mode=print;;
    *) die "unknown flag $f (the collector flags are --collector.<name>, --no-collector.<name>, --collector.disable-defaults)";;
  esac
done
for c in $(echo "$explicit" | awk 'NF { print $1 }' | sort -u); do
  echo "$available" | grep -qx "$c" || die "no collector $c; there are: $(echo "$available" | tr '\n' ' ')"
done

state_of() {
  s=$(echo "$explicit" | awk -v c="$1" '$1 == c { s = $2 } END { print s }')
  if echo "$forced" | grep -qx "$1"; then
    [ "$s" != off ] || die "the $1 collector cannot be turned off"
    echo forced; return
  fi
  if [ -n "$s" ]; then echo "$s"; return; fi
  if ! $disable_defaults && echo "$defaults" | grep -qx "$1"; then echo on; else echo off; fi
}

enabled=
for c in $available; do
  s=$(state_of "$c")
  if [ "$mode" = list ]; then
    d=; echo "$defaults" | grep -qx "$c" && [ "$s" != forced ] && d=", on by default"
    echo "$c $s$d"
  fi
  [ "$s" = off ] || enabled="$enabled $c"
done
[ "$mode" != list ] || exit 0

# the parts the enabled collectors need: "# needs: <part> ..." lines
parts=$(for c in $enabled; do sed -n 's/^# needs:[[:space:]]*//p' "$etc/collectors.d/$c.yaml"; done | tr -s ' \t' '\n' | sed '/^$/d' | sort -u)
for p in $parts; do
  [ -f "$etc/parts.d/$p.yaml" ] || die "a collector needs the part $p, which has no parts.d/$p.yaml"
done

config() {
  echo "# Written by observability-host-launch at every start; change the"
  echo "# collectors with $args_file (HOST_MODE.md, Collectors), not here."
  echo "# collectors:$enabled"
  echo "includes:"
  echo "  - $etc/host.yaml"
  for p in $parts; do echo "  - $etc/parts.d/$p.yaml"; done
  for c in $enabled; do echo "  - $etc/collectors.d/$c.yaml"; done
}

if [ "$mode" = print ]; then
  config
  exit 0
fi

# written whole, then moved, so that a crash leaves the previous one
mkdir -p "$state"
config > "$state/fluent-bit.yaml.tmp"
mv "$state/fluent-bit.yaml.tmp" "$state/fluent-bit.yaml"
echo "observability-host-launch: collectors:$enabled" >&2
exec "$fluent_bit" -c "$state/fluent-bit.yaml" "$@"
