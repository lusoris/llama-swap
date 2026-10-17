#!/usr/bin/env bash
# Compare gocyclo or gocognit output with the committed baseline.
#
#   complexity-ratchet.sh TOOL FINDINGS STDERR BASELINE           check
#   complexity-ratchet.sh --record TOOL FINDINGS STDERR BASELINE  rewrite TOOL's rows
#
# FINDINGS holds the tool's "-over N" lines: <score> <package> <function>
# <file:line:column>. A function fails the check when the baseline has no row
# for it or records a lower score; every finding is printed. Rows key on
# file, package and function, not on the line, so moving code keeps its row.
# The tools' exit codes do not tell an error from a finding (gocyclo exits 0
# on a missing path), so any STDERR line other than gocognit's recursion note
# fails the check.
set -euo pipefail

record=0
if [ "${1:-}" = --record ]; then
  record=1
  shift
fi
[ $# -eq 4 ] || {
  echo "usage: $0 [--record] TOOL FINDINGS STDERR BASELINE" >&2
  exit 2
}
tool=$1 findings=$2 stderr=$3 baseline=$4

if grep -v '^gocognit: note: ' "$stderr" | grep -q .; then
  echo "::error::$tool reported errors:" >&2
  cat "$stderr" >&2
  exit 2
fi

current=$(mktemp)
trap 'rm -f "$current"' EXIT
awk -v tool="$tool" '
  NF != 4 || $1 !~ /^[0-9]+$/ { print "unparsed line: " $0 > "/dev/stderr"; bad = 1; next }
  { pos = $4; sub(/:[0-9]+:[0-9]+$/, "", pos); print tool, $1, pos, $2, $3 }
  END { exit bad }
' "$findings" | sort -k3,5 -k2,2nr | awk '!seen[$3 " " $4 " " $5]++' > "$current"

if [ "$record" -eq 1 ]; then
  # Keep the other tool's rows; grep exits 1 when there are none.
  {
    if [ -f "$baseline" ]; then grep -v "^$tool " "$baseline" || [ $? -eq 1 ]; fi
    cat "$current"
  } | sort -k1,1 -k3,5 > "$baseline.new"
  mv "$baseline.new" "$baseline"
  echo "$tool: recorded $(wc -l < "$current") rows in $baseline"
  exit 0
fi

[ -f "$baseline" ] || {
  echo "::error::baseline $baseline is missing" >&2
  exit 2
}
awk -v tool="$tool" '
  FILENAME == ARGV[1] { if ($1 == tool) base[$3 " " $4 " " $5] = $2; next }
  {
    n++
    key = $3 " " $4 " " $5
    if (!(key in base)) { print "::error file=" $3 "::" tool " " $2 " " $4 "." $5 " (new, not in the baseline)"; grew++ }
    else if ($2 + 0 > base[key] + 0) { print "::error file=" $3 "::" tool " " $2 " " $4 "." $5 " (baseline " base[key] ")"; grew++ }
    else { print tool " " $2 " " $4 "." $5 " (baselined " base[key] ")" }
    seen[key] = 1
  }
  END {
    for (k in base) if (!(k in seen)) fell++
    printf "%s: %d findings, %d above the baseline, %d baseline rows no longer reached\n", tool, n, grew, fell
    exit grew > 0
  }
' "$baseline" "$current"
