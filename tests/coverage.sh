#!/usr/bin/env bash
# Coverage of the unit tests and the smoke test together: the smoke test runs
# a binary built with coverage on, so cmd/netmon and the MQTT publisher (which
# have no unit tests of their own) are measured against a real broker.
#
#   tests/coverage.sh [OUT_DIR]     default ./coverage
#
# Writes OUT_DIR/coverage.out (go tool cover's text format), coverage.html,
# and coverage.txt (per function, then per package), and checks the floors
# below. Fails if any test fails or any floor is missed.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE/.."
OUT="$(mkdir -p "${1:-coverage}" && cd "${1:-coverage}" && pwd)"
RAW="$(mktemp -d "${TMPDIR:-/tmp}/netmon-cover.XXXXXX")"
trap 'rm -rf "$RAW"' EXIT
mkdir -p "$RAW/unit" "$RAW/smoke"

# Packages held at 100%: the pure logic that decides what gets published.
FULL=(internal/classify internal/config internal/payload)
# Everything together must not drop below this.
TOTAL_FLOOR=90

echo "### unit tests (coverage across all packages)"
go test -count=1 -cover -coverpkg=./... ./... -args -test.gocoverdir="$RAW/unit" | grep -v 'no test files'
echo "### smoke test (binary built with -cover)"
GOCOVERDIR="$RAW/smoke" tests/smoke.sh

go tool covdata textfmt -i="$RAW/unit,$RAW/smoke" -o "$OUT/coverage.out"
go tool cover -html="$OUT/coverage.out" -o "$OUT/coverage.html"
go tool cover -func="$OUT/coverage.out" > "$OUT/coverage.txt"
{ echo; echo "per package:"; go tool covdata percent -i="$RAW/unit,$RAW/smoke"; } >> "$OUT/coverage.txt"

fails=0
pkg_pct() {  # statement coverage of one package, from the merged profile
  awk -v p="github.com/sweeney/netmon/$1/" 'NR > 1 && index($1, p) == 1 && substr($1, length(p) + 1) !~ /\// {
      n = $2; total += n; if ($3 > 0) hit += n }
    END { if (total) printf "%.1f", 100 * hit / total; else print "0.0" }' "$OUT/coverage.out"
}
echo "### coverage"
for p in "${FULL[@]}"; do
  pct="$(pkg_pct "$p")"
  if [ "$pct" = "100.0" ]; then echo "  ok    $p 100%"
  else echo "  FAIL  $p ${pct}%, expected 100%"; fails=$((fails + 1)); fi
done
total="$(awk '/^total:/ {sub(/%/, "", $NF); print $NF}' "$OUT/coverage.txt")"
if awk -v t="$total" -v f="$TOTAL_FLOOR" 'BEGIN { exit !(t >= f) }'; then echo "  ok    total ${total}% (floor ${TOTAL_FLOOR}%)"
else echo "  FAIL  total ${total}%, below the ${TOTAL_FLOOR}% floor"; fails=$((fails + 1)); fi
echo "written: $OUT/coverage.out coverage.html coverage.txt"
[ "$fails" -eq 0 ]
