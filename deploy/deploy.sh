#!/usr/bin/env bash
# Run from your Mac. Builds netmon for the target host, copies it with this
# directory and the site's config, and runs deploy/remote.sh there (under
# sudo, which asks for your password).
#
#   deploy/deploy.sh [--site NAME] --check     what is installed, what would change (read-only, no sudo)
#   deploy/deploy.sh [--site NAME]             install / upgrade; shows the plan and asks first
#   deploy/deploy.sh [--site NAME] --yes       ... without asking
#   deploy/deploy.sh [--site NAME] rollback [--check]   stop and remove netmon (config and state kept)
#
#   --site NAME        a site in deploy/sites, with its config in configs/NAME.toml
#                      (default schoolhouse)
#   --host USER@HOST   override the site's ssh target
#   --hub HOST         override the site's hub (where the install is verified)
#
# deploy/sites is local and not in git: copy deploy/sites.example and fill in
# your hosts. NETMON_SITES names another file.
#
# The binary is built for whatever `uname -m` the host reports.
set -euo pipefail

SRC="$(cd "$(dirname "$0")/.." && pwd)"
SITE="schoolhouse" HOST="" HUB="" ARGS=() CHECK=0
while [ $# -gt 0 ]; do
  case "$1" in
    --site) SITE="${2:?--site needs a name}"; shift ;;
    --host) HOST="${2:?--host needs USER@HOST}"; shift ;;
    --hub) HUB="${2:?--hub needs a host}"; shift ;;
    --check|--dry-run) CHECK=1; ARGS+=(--check) ;;
    -h|--help) sed -n '2,19p' "$0"; exit 0 ;;
    *) ARGS+=("$1") ;;
  esac
  shift
done

die() { echo "ERROR: $*" >&2; exit 1; }

CONFIG="$SRC/configs/$SITE.toml"
[ -f "$CONFIG" ] || die "no config for site '$SITE' (expected configs/$SITE.toml)"
SITES="${NETMON_SITES:-$SRC/deploy/sites}"
if [ -z "$HOST" ] || [ -z "$HUB" ]; then
  [ -f "$SITES" ] || die "no $SITES: copy deploy/sites.example to deploy/sites and fill in your hosts (or pass --host and --hub)"
  line="$(awk -v s="$SITE" '$1 == s {print $2, $3; exit}' "$SITES")"
  [ -n "$line" ] || die "site '$SITE' is not in $SITES; add it or pass --host and --hub"
  read -r site_host site_hub <<<"$line"
  HOST="${HOST:-$site_host}" HUB="${HUB:-$site_hub}"
  [ -n "$HUB" ] || die "site '$SITE' in $SITES has no hub (third column)"
fi

ssh -o ConnectTimeout=10 -o BatchMode=yes "$HOST" true || die "cannot ssh to $HOST"
ARCH="$(ssh "$HOST" uname -m)"
case "$ARCH" in
  aarch64|arm64) GOARCH=arm64 ;;
  x86_64|amd64)  GOARCH=amd64 ;;
  *) die "$HOST is $ARCH; netmon is built for arm64 and amd64 only" ;;
esac

STAGE="$(mktemp -d "${TMPDIR:-/tmp}/netmon-deploy.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT
VERSION="$(git -C "$SRC" describe --always --dirty 2>/dev/null || echo dev)"
echo "site $SITE on $HOST: building netmon $VERSION for linux/$GOARCH"
(cd "$SRC" && GOOS=linux GOARCH="$GOARCH" CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$VERSION" -o "$STAGE/netmon" ./cmd/netmon)
mkdir -p "$STAGE/deploy"
cp "$SRC/deploy/remote.sh" "$SRC/deploy/netmon.service" "$SRC/deploy/shipped-configs.sha256" "$STAGE/deploy/"
cp "$CONFIG" "$STAGE/config.toml.example"   # remote.sh installs it if the host has no config

REMOTE_DIR="$(ssh "$HOST" mktemp -d /tmp/netmon.XXXXXX)"
trap 'rm -rf "$STAGE"; ssh "$HOST" rm -rf "$REMOTE_DIR" >/dev/null 2>&1 || true' EXIT
tar -C "$STAGE" -cf - . | ssh "$HOST" tar -C "$REMOTE_DIR" -xf -

echo "--- $HOST ---"
if [ "$CHECK" = 1 ]; then
  ssh "$HOST" bash "$REMOTE_DIR/deploy/remote.sh" --hub "$HUB" ${ARGS[@]+"${ARGS[@]}"}
else
  ssh -t "$HOST" sudo bash "$REMOTE_DIR/deploy/remote.sh" --hub "$HUB" ${ARGS[@]+"${ARGS[@]}"}
fi
