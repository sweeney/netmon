#!/usr/bin/env bash
# Runs ON THE PI. Normally invoked by deploy.sh, which builds netmon, copies
# this tree over and runs it under sudo.
#
#   remote.sh [--check] [--yes] install    install / upgrade (default)
#   remote.sh [--check] [--yes] rollback   stop and remove it (config and state kept)
#
# --check  report what is installed and what would change; changes nothing and
#          needs no root.
# --yes    skip the confirmation prompt.
#
# The config is installed only if there is none: edits on the Pi survive
# upgrades. Safe to re-run: a run with nothing to change restarts nothing.
#
# ROOT (env) prefixes every path, for the tests. Empty on the Pi.
set -euo pipefail

ROOT="${ROOT:-}"
CONFIRM_TTY="${CONFIRM_TTY:-/dev/tty}"
HERE="$(cd "$(dirname "$0")" && pwd)"
PAYLOAD="$(cd "$HERE/.." && pwd)"

GARIBALDI_IP="${GARIBALDI_IP:-100.64.0.20}"   # verification reads the bridged copy there

BIN="$ROOT/usr/local/bin/netmon"
CFG_DIR="$ROOT/etc/netmon"
CFG="$CFG_DIR/config.toml"
UNIT="$ROOT/etc/systemd/system/netmon.service"
BACKUP_ROOT="$ROOT/var/backups/netmon"

CHECK=0 YES=0 CMD=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check|--dry-run) CHECK=1 ;;
    --yes|-y) YES=1 ;;
    install|rollback) CMD="$1" ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
CMD="${CMD:-install}"

say()  { echo "==> $*"; }
info() { echo "    $*"; }
die()  { echo "ERROR: $*" >&2; exit 1; }

confirm() {
  [ "$YES" = 1 ] && return 0
  if ! { exec 3<"$CONFIRM_TTY"; } 2>/dev/null; then
    die "no terminal to confirm on; re-run with --yes to proceed without a prompt"
  fi
  local reply; printf '%s [y/N] ' "$1"; read -r reply <&3 || true; exec 3<&-
  [[ "$reply" =~ ^[Yy]$ ]] || { echo "aborted, nothing changed"; exit 1; }
}
need_root() { [ "$(id -u)" = 0 ] || die "$CMD needs root (deploy.sh runs it under sudo)"; }
same_file() { [ -f "$2" ] && cmp -s "$1" "$2"; }

# The site id and topic, from the config that will be in force.
site_id() { awk -F'"' '/^\[site\]/{s=1;next} /^\[/{s=0} s && /^[[:space:]]*id[[:space:]]*=/{print $2; exit}' "$1"; }

BACKUP=""
backup() {
  if [ -z "$BACKUP" ]; then
    # Unique per run: two runs in the same second must not share (and nest) one.
    mkdir -p "$BACKUP_ROOT"; BACKUP="$(mktemp -d "$BACKUP_ROOT/$(date +%Y%m%d-%H%M%S).XXXX")"
    info "backing up to ${BACKUP#"$ROOT"}"
  fi
  local p; for p in "$@"; do
    [ -e "$p" ] || continue
    mkdir -p "$BACKUP$(dirname "${p#"$ROOT"}")"
    cp -p "$p" "$BACKUP${p#"$ROOT"}"
  done
}

# ---------------------------------------------------------------------------
preflight() {
  say "preflight"
  local id; id="$(. "$ROOT/etc/os-release" && echo "$ID $VERSION_ID")"
  [ "$id" = "debian 13" ] || die "expected Debian 13, found: $id"
  info "OS: $id"
  for f in netmon deploy/netmon.service config.toml.example; do
    [ -f "$PAYLOAD/$f" ] || die "payload missing $f"
  done
  local v; v="$("$PAYLOAD/netmon" -version 2>/dev/null)" || die "the payload binary does not run here (wrong architecture?)"
  info "payload: netmon $v"
  local range; range="$(cat "$ROOT/proc/sys/net/ipv4/ping_group_range" 2>/dev/null || echo "1 0")"
  # shellcheck disable=SC2086
  set -- $range
  [ "$1" -le 0 ] && [ "$2" -ge 2147483647 ] \
    || die "net.ipv4.ping_group_range is '$range': unprivileged ping would fail for netmon's dynamic user"
  info "unprivileged ICMP allowed for every group"
  systemctl is-active --quiet mosquitto || die "the local mosquitto is not running (pi-setup/mqtt-bridge installs it)"
  info "local mosquitto active"
}

PENDING=()
plan_install() {
  PENDING=()
  same_file "$PAYLOAD/netmon" "$BIN" || PENDING+=("install /usr/local/bin/netmon ($("$PAYLOAD/netmon" -version), now: $([ -x "$BIN" ] && "$BIN" -version 2>/dev/null || echo none))")
  [ -f "$CFG" ] || PENDING+=("install /etc/netmon/config.toml from config.toml.example (kept on later upgrades)")
  same_file "$PAYLOAD/deploy/netmon.service" "$UNIT" || PENDING+=("install /etc/systemd/system/netmon.service")
  if [ "$(systemctl is-enabled netmon 2>/dev/null)" != enabled ] || ! systemctl is-active --quiet netmon || [ ${#PENDING[@]} -gt 0 ]; then
    PENDING+=("enable + (re)start netmon")
  fi
}

verify() {  # $1: config in force
  say "verifying"
  local site topic got i
  site="$(site_id "$1")"; topic="sites/$site/network/state"
  for i in $(seq 10); do systemctl is-active --quiet netmon && break; sleep 1; done
  systemctl is-active --quiet netmon || { echo "    netmon is not active"; return 1; }
  for i in $(seq 20); do
    journalctl -u netmon --since "@$STARTED_AT" --no-pager 2>/dev/null | grep -q "connected to MQTT broker" && break
    sleep 1
  done
  journalctl -u netmon --since "@$STARTED_AT" --no-pager 2>/dev/null | grep -q "connected to MQTT broker" \
    || { echo "    netmon has not logged 'connected to MQTT broker' since its start"; return 1; }
  info "netmon connected to the local broker"
  got="$(mosquitto_sub -h "$GARIBALDI_IP" -t "$topic" -R -C 1 -W 30 2>/dev/null || true)"
  [ -n "$got" ] || { echo "    no live $topic on garibaldi within 30 s"; return 1; }
  case "$got" in *'"online":true'*) ;; *) echo "    $topic on garibaldi is not online: $got"; return 1 ;; esac
  info "garibaldi received a live $topic via the bridge: status $(sed -n 's/.*"status":"\([^"]*\)".*/\1/p' <<<"$got")"
}

restore() {
  echo "!!> verification failed; restoring the previous netmon" >&2
  if [ -n "$BACKUP" ] && [ -f "$BACKUP/usr/local/bin/netmon" ]; then cp -p "$BACKUP/usr/local/bin/netmon" "$BIN"; fi
  if [ -n "$BACKUP" ] && [ -f "$BACKUP/etc/systemd/system/netmon.service" ]; then
    cp -p "$BACKUP/etc/systemd/system/netmon.service" "$UNIT"
    systemctl daemon-reload; systemctl restart netmon || true
    echo "    previous version restored and restarted" >&2
  else
    systemctl disable --now netmon 2>/dev/null || true
    rm -f "$UNIT" "$BIN"; systemctl daemon-reload
    echo "    netmon removed (it was not installed before); config kept" >&2
  fi
  echo "    log: journalctl -u netmon" >&2
}

do_install() {
  [ "$CHECK" = 1 ] || need_root
  preflight
  plan_install
  say "plan"
  if [ ${#PENDING[@]} -eq 0 ]; then info "nothing to do: netmon $("$BIN" -version) installed and running"; return 0; fi
  local p; for p in "${PENDING[@]}"; do info "- $p"; done
  if [ "$CHECK" = 1 ]; then echo "(--check: nothing changed)"; return 0; fi
  confirm "Apply these changes on $(hostname)?"

  backup "$BIN" "$UNIT"
  say "installing"
  if ! same_file "$PAYLOAD/netmon" "$BIN"; then
    install -m 755 "$PAYLOAD/netmon" "$BIN.netmon-new"; mv -f "$BIN.netmon-new" "$BIN"; info "installed /usr/local/bin/netmon"
  fi
  if [ ! -f "$CFG" ]; then
    mkdir -p "$CFG_DIR"; install -m 644 "$PAYLOAD/config.toml.example" "$CFG"; info "installed /etc/netmon/config.toml"
  fi
  if ! same_file "$PAYLOAD/deploy/netmon.service" "$UNIT"; then
    install -m 644 "$PAYLOAD/deploy/netmon.service" "$UNIT"; info "installed netmon.service"
  fi
  "$BIN" -config "$CFG" -check-config >/dev/null || { echo "!!> /etc/netmon/config.toml does not load" >&2; restore; exit 1; }
  systemctl daemon-reload
  systemctl enable netmon >/dev/null 2>&1
  STARTED_AT="$(date +%s)"
  systemctl restart netmon
  verify "$CFG" || { restore; exit 1; }
  say "done"
  info "watch it: journalctl -u netmon -f"
}

do_rollback() {
  [ "$CHECK" = 1 ] || need_root
  PENDING=()
  { systemctl is-active --quiet netmon || [ "$(systemctl is-enabled netmon 2>/dev/null)" = enabled ]; } && PENDING+=("stop + disable netmon")
  [ -f "$UNIT" ] && PENDING+=("remove /etc/systemd/system/netmon.service")
  [ -f "$BIN" ] && PENDING+=("remove /usr/local/bin/netmon")
  say "rollback plan"
  if [ ${#PENDING[@]} -eq 0 ]; then info "nothing to do: netmon is not installed"; return 0; fi
  local p; for p in "${PENDING[@]}"; do info "- $p"; done
  info "(kept: /etc/netmon/config.toml, /var/lib/private/netmon)"
  if [ "$CHECK" = 1 ]; then echo "(--check: nothing changed)"; return 0; fi
  confirm "Remove netmon from $(hostname)?"
  backup "$BIN" "$UNIT"
  systemctl disable --now netmon 2>/dev/null || true
  rm -f "$UNIT" "$BIN"
  systemctl daemon-reload
  # Its retained topics would otherwise say "offline" (or an outage) forever.
  if [ -f "$CFG" ] && command -v mosquitto_pub >/dev/null; then
    local site leaf; site="$(site_id "$CFG")"
    for leaf in state outage last_outage; do
      mosquitto_pub -h 127.0.0.1 -t "sites/$site/network/$leaf" -r -n -q 1 || true
    done
    info "cleared sites/$site/network/{state,outage,last_outage}"
  fi
  say "removed"; info "previous files kept in ${BACKUP#"$ROOT"}"
}

case "$CMD" in
  install) do_install ;;
  rollback) do_rollback ;;
esac
