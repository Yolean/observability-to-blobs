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
#   HOST_REQUIRED_FILE
#                    the collectors this host requires, one or more names a
#                    line, which no flag turns off, as the heartbeat
#                    (default: required beside the args file; a missing file
#                    requires none)
#   HOST_ETC         host.yaml, collectors.defaults, collectors.d/, parts.d/
#                    (default: etc/ beside this script's bin/)
#   HOST_COLLECTOR_DIRS
#                    more directories of collectors, separated by colons,
#                    each with collectors.d/ and, if it has any, parts.d/
#                    and collectors.defaults, whose collectors add to
#                    HOST_ETC's defaults; a collector name may be in only
#                    one directory. The machine's own, collectors/ beside
#                    the args file (/etc/observability-host/collectors/),
#                    comes after them when it has a collectors.d/
#   STATE_DIRECTORY  the state directory: the configuration, and everything
#                    fluent-bit and the heartbeat keep (systemd sets it from
#                    StateDirectory=; default /var/lib/observability-host).
#                    Exported to fluent-bit as HOST_STATE_DIR, which the
#                    collectors' paths use, and to the heartbeat as
#                    HEARTBEAT_STATE_DIR unless that is set
#   HOST_FLUENT_BIT  the binary (default: fluent-bit beside this script)
set -eu

bin=$(cd "$(dirname "$0")" && pwd)
etc=${HOST_ETC:-$(cd "$bin/../etc" 2>/dev/null && pwd || echo "$bin/../etc")}
state=${STATE_DIRECTORY:-/var/lib/observability-host}
args_file=${HOST_ARGS_FILE:-/etc/observability-host/args}
required_file=${HOST_REQUIRED_FILE:-$(dirname "$args_file")/required}
fluent_bit=${HOST_FLUENT_BIT:-$bin/fluent-bit}

die() { echo "observability-host-launch: $*" >&2; exit 2; }

[ -f "$etc/host.yaml" ] || die "no host.yaml in $etc (HOST_ETC)"
[ -f "$etc/collectors.defaults" ] || die "no collectors.defaults in $etc (HOST_ETC)"

# the directories of collectors: HOST_ETC, then HOST_COLLECTOR_DIRS'
roots=$etc
old_ifs=$IFS; IFS=:
for d in ${HOST_COLLECTOR_DIRS:-}; do
  [ -n "$d" ] || continue
  [ -d "$d/collectors.d" ] || die "HOST_COLLECTOR_DIRS names $d, which has no collectors.d/"
  roots="$roots:$(cd "$d" && pwd)"
done
IFS=$old_ifs
# the machine's own collectors, beside its args (HOST_MODE.md, Your own
# collectors); once, if HOST_COLLECTOR_DIRS names it too
own=$(dirname "$args_file")/collectors
if [ -d "$own/collectors.d" ]; then
  own=$(cd "$own" && pwd)
  case ":$roots:" in *":$own:"*) ;; *) roots="$roots:$own" ;; esac
fi

# every collector there is, "<name> <file>": <root>/collectors.d/<name>.yaml
index=$(IFS=:; for r in $roots; do
  # an empty collectors.d/ leaves the pattern itself, which is no file
  for f in "$r"/collectors.d/*.yaml; do [ ! -e "$f" ] || echo "$(basename "$f" .yaml) $f"; done
done)
dup=$(echo "$index" | awk 'NF { print $1 }' | sort | uniq -d | tr '\n' ' ')
[ -z "$dup" ] || die "collectors in more than one directory: $dup"
available=$(echo "$index" | awk 'NF { print $1 }' | sort)
file_of() { echo "$index" | awk -v c="$1" '$1 == c { print $2 }'; }
defaults_lines=$(IFS=:; for r in $roots; do [ ! -f "$r/collectors.defaults" ] || sed -e 's/#.*//' "$r/collectors.defaults"; done)
defaults=$(echo "$defaults_lines" | awk 'NF { print $1 }')
forced=$(echo "$defaults_lines" | awk 'NF && $2 == "forced" { print $1 }')
for c in $defaults; do
  echo "$available" | grep -qx "$c" || die "a collectors.defaults names $c, which has no collectors.d/$c.yaml"
done
# the installer's: collectors that must run on this host, whatever the
# flags say (HOST_MODE.md, Collectors)
required=
if [ -f "$required_file" ]; then
  required=$(sed -e 's/#.*//' "$required_file" | tr -s ' \t' '\n' | sed '/^$/d')
  for c in $required; do
    echo "$available" | grep -qx "$c" || die "$required_file requires $c, which has no collectors.d/$c.yaml"
  done
fi

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
  if echo "$required" | grep -qx "$1"; then
    [ "$s" != off ] || die "the $1 collector is required on this host ($required_file)"
    echo required; return
  fi
  if [ -n "$s" ]; then echo "$s"; return; fi
  if ! $disable_defaults && echo "$defaults" | grep -qx "$1"; then echo on; else echo off; fi
}

enabled=
for c in $available; do
  s=$(state_of "$c")
  if [ "$mode" = list ]; then
    d=; echo "$defaults" | grep -qx "$c" && [ "$s" != forced ] && [ "$s" != required ] && d=", on by default"
    echo "$c $s$d"
  fi
  [ "$s" = off ] || enabled="$enabled $c"
done
[ "$mode" != list ] || exit 0

# the parts the enabled collectors need: "# needs: <part> ..." lines
parts=$(for c in $enabled; do sed -n 's/^# needs:[[:space:]]*//p' "$(file_of "$c")"; done | tr -s ' \t' '\n' | sed '/^$/d' | sort -u)
# part_file <part>: the first directory's parts.d/<part>.yaml
part_file() { (IFS=:; for r in $roots; do [ ! -f "$r/parts.d/$1.yaml" ] || { echo "$r/parts.d/$1.yaml"; exit; }; done); }
for p in $parts; do
  [ -n "$(part_file "$p")" ] || die "a collector needs the part $p, which no parts.d/ has"
done

config() {
  echo "# Written by observability-host-launch at every start; change the"
  echo "# collectors with $args_file (HOST_MODE.md, Collectors), not here."
  echo "# collectors:$enabled"
  echo "includes:"
  echo "  - $etc/host.yaml"
  for p in $parts; do echo "  - $(part_file "$p")"; done
  for c in $enabled; do echo "  - $(file_of "$c")"; done
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
HOST_STATE_DIR=$state
HEARTBEAT_STATE_DIR=${HEARTBEAT_STATE_DIR:-$state}
export HOST_STATE_DIR HEARTBEAT_STATE_DIR
# for the heartbeat, which ships them in its row (HOST_MODE.md, What it ships)
HOST_COLLECTORS=$(echo "${enabled# }" | tr ' ' ',')
export HOST_COLLECTORS
exec "$fluent_bit" -c "$state/fluent-bit.yaml" "$@"
