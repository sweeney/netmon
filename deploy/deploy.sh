#!/usr/bin/env bash
# Run from your Mac. Builds netmon for the Pi, copies it with this directory,
# and runs deploy/remote.sh there (under sudo, which asks for your password).
#
#   deploy/deploy.sh --check              what is installed, what would change (read-only, no sudo)
#   deploy/deploy.sh                      install / upgrade; shows the plan and asks first
#   deploy/deploy.sh --yes                ... without asking
#   deploy/deploy.sh rollback [--check]   stop and remove netmon (config and state kept)
#
#   --host USER@HOST   default sweeney@100.64.0.10 (pi3b-schoolhouse)
set -euo pipefail

HOST="${PI_HOST:-sweeney@100.64.0.10}"
ARGS=() CHECK=0
while [ $# -gt 0 ]; do
  case "$1" in
    --host) HOST="${2:?--host needs USER@HOST}"; shift ;;
    --check|--dry-run) CHECK=1; ARGS+=(--check) ;;
    -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
    *) ARGS+=("$1") ;;
  esac
  shift
done

SRC="$(cd "$(dirname "$0")/.." && pwd)"
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/netmon-deploy.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT

VERSION="$(git -C "$SRC" describe --always --dirty 2>/dev/null || echo dev)"
echo "building netmon $VERSION for linux/arm64"
(cd "$SRC" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$VERSION" -o "$STAGE/netmon" ./cmd/netmon)
mkdir -p "$STAGE/deploy"
cp "$SRC/deploy/remote.sh" "$SRC/deploy/netmon.service" "$STAGE/deploy/"
cp "$SRC/config.toml.example" "$STAGE/"

ssh -o ConnectTimeout=10 -o BatchMode=yes "$HOST" true \
  || { echo "ERROR: cannot ssh to $HOST" >&2; exit 1; }
REMOTE_DIR="$(ssh "$HOST" mktemp -d /tmp/netmon.XXXXXX)"
trap 'rm -rf "$STAGE"; ssh "$HOST" rm -rf "$REMOTE_DIR" >/dev/null 2>&1 || true' EXIT
tar -C "$STAGE" -cf - . | ssh "$HOST" tar -C "$REMOTE_DIR" -xf -

echo "--- $HOST ---"
if [ "$CHECK" = 1 ]; then
  ssh "$HOST" bash "$REMOTE_DIR/deploy/remote.sh" ${ARGS[@]+"${ARGS[@]}"}
else
  ssh -t "$HOST" sudo bash "$REMOTE_DIR/deploy/remote.sh" ${ARGS[@]+"${ARGS[@]}"}
fi
