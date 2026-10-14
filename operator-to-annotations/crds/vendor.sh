#!/usr/bin/env bash
# The prometheus-operator CRDs of this base, pinned in kustomization.yaml
# by release tag and sha256 per file (../README.md, "CRDs"). Each CRD is a
# base of its own, <plural>/, holding the release's file.
#
#   vendor.sh check     every CRD base's file is pinned and matches its
#                       sha256, and the bases serve v1 and nothing else
#   vendor.sh <tag>     download the same files at that release, unmodified,
#                       move the pins, then check
#
# A release that adds another served version fails the check: strip that
# version with a patch in the CRD's kustomization.yaml, leaving the file as
# released.
set -euo pipefail
cd "$(dirname "$0")"

SRC=https://raw.githubusercontent.com/prometheus-operator/prometheus-operator
DIR=example/prometheus-operator-crd

fail() { echo "[vendor.sh] $*" >&2; exit 1; }
# the CRD bases, and the file of each
bases() { sed -n 's/^- \([a-z]*\)$/\1/p' kustomization.yaml; }
files() { local b; for b in $(bases); do echo "$b/monitoring.coreos.com_$b.yaml"; done; }

check() {
  local f want got pinned b names
  pinned=$(sed -n 's/^# sha256: [0-9a-f]\{64\}  //p' kustomization.yaml)
  [ "$(files)" = "$pinned" ] || fail "the sha256 lines do not list the CRD bases' files, in order"
  for f in $(files); do
    want=$(sed -n "s|^# sha256: \([0-9a-f]\{64\}\)  ${f//./\\.}\$|\1|p" kustomization.yaml)
    got=$(sha256sum "$f" | cut -d' ' -f1)
    [ "$want" = "$got" ] || fail "$f: sha256 $got, pinned $want"
  done
  # controller-gen writes a version's name either first in its list item
  # or right under the item's first key; nothing deeper is at either indent
  for b in $(bases); do
    names=$(kubectl kustomize "$b" | sed -n 's/^\(  - \|    \)name: //p' | sort -u)
    [ "$names" = v1 ] || fail "$b serves $(echo "$names" | tr '\n' ' '), want only v1"
  done
  echo "[vendor.sh] $(bases | wc -l) CRDs at $(sed -n 's/^# release: //p' kustomization.yaml), v1 only"
}

bump() {
  local tag=$1 f sum
  for f in $(files); do
    curl -sSfLo "$f.new" "$SRC/$tag/$DIR/${f#*/}" || fail "${f#*/} not found at $tag"
    mv "$f.new" "$f"
    sum=$(sha256sum "$f" | cut -d' ' -f1)
    sed -i "s|^# sha256: [0-9a-f]\{64\}  ${f//./\\.}\$|# sha256: $sum  $f|" kustomization.yaml
  done
  sed -i "s/^# release: .*/# release: $tag/" kustomization.yaml
}

case "${1:-}" in
  check) check ;;
  v[0-9]*) bump "$1"; check ;;
  *) echo "usage: $0 check|<release tag>" >&2; exit 2 ;;
esac
