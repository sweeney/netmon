#!/usr/bin/env bash
# Runs every test: Go unit and daemon tests, the deploy scripts against
# fixtures, and the binary against a real mosquitto (inside coverage.sh,
# which also checks the coverage floors). About 1½ minutes. CI runs the same
# steps (.github/workflows/ci.yml).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
export PATH="/opt/homebrew/sbin:$PATH"
cd "$HERE/.."
for f in deploy/*.sh tests/*.sh; do bash -n "$f"; done && echo "syntax: ok"
[ -z "$(gofmt -l .)" ] || { echo "gofmt:"; gofmt -l .; exit 1; }
go vet ./...
echo "### go test -race"; go test -race -count=1 ./... 2>&1 | grep -v 'no test files'
echo "### remote-script.sh"; tests/remote-script.sh
echo "### deploy-script.sh"; tests/deploy-script.sh
echo "### coverage.sh (unit tests again, and smoke.sh)"; tests/coverage.sh "${TMPDIR:-/tmp}/netmon-coverage"
