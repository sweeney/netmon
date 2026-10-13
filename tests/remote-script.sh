#!/usr/bin/env bash
# Tests deploy/remote.sh against a fixture root. systemctl, journalctl, the
# mosquitto clients, id and sleep are shims that record calls and play back a
# Pi's state; the payload "binary" is a script. FAIL_VERIFY makes a
# verification step fail so the restore path runs.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SRC="$HERE/.."
BASH_BIN="${BASH_BIN:-$(command -v bash)}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/netmon-remote-test.XXXXXX")"
fails=0
trap '[ "$fails" -gt 0 ] && echo "work dir kept: $WORK" || rm -rf "$WORK"' EXIT
pass() { echo "  PASS  $*"; }
fail() { echo "  FAIL  $*"; fails=$((fails + 1)); }
check() { local d="$1"; shift; if "$@"; then pass "$d"; else fail "$d"; fi; }
not() { ! "$@"; }

SHIMS="$WORK/shims"; mkdir -p "$SHIMS"
shim() { printf '#!/usr/bin/env bash\nset -u\n%s\n' "$2" > "$SHIMS/$1"; chmod +x "$SHIMS/$1"; }
shim id '[ "${1:-}" = -u ] && { echo 0; exit 0; }; /usr/bin/id "$@"'
shim sleep 'true'
shim systemctl '
echo "systemctl $*" >> "$STATE/calls"
svc="${!#}"
case "$1" in
  is-active) [ -f "$STATE/svc-$svc-active" ] ;;
  is-enabled) if [ -f "$STATE/svc-$svc-enabled" ]; then echo enabled; else echo disabled; exit 1; fi ;;
  restart) [ "${FAIL_VERIFY:-}" = start ] || touch "$STATE/svc-$svc-active"; date +%s > "$STATE/started" ;;
  enable) touch "$STATE/svc-$svc-enabled" ;;
  disable) rm -f "$STATE/svc-$svc-enabled"; [ "$2" = --now ] && rm -f "$STATE/svc-$svc-active"; true ;;
  *) true ;;
esac'
shim journalctl '[ "${FAIL_VERIFY:-}" = journal ] || echo "netmon[1]: connected to MQTT broker tcp://127.0.0.1:1883"'
shim mosquitto_sub '
[ "${FAIL_VERIFY:-}" = garibaldi ] && exit 27
echo "{\"timestamp\":\"2026-10-13T22:00:00Z\",\"online\":true,\"status\":\"ok\"}"'
shim mosquitto_pub 'echo "mosquitto_pub $*" >> "$STATE/calls"'

# A payload tree as deploy.sh stages it, with a script for the binary.
make_payload() {  # $1 dir, $2 version
  mkdir -p "$1/deploy"
  cp "$SRC/deploy/remote.sh" "$SRC/deploy/netmon.service" "$1/deploy/"
  cp "$SRC/config.toml.example" "$1/"
  cat > "$1/netmon" <<EOF
#!/usr/bin/env bash
for a in "\$@"; do
  case "\$a" in
    -version) echo $2; exit 0 ;;
    -check-config) [ "\${FAIL_VERIFY:-}" = config ] && { echo "bad config" >&2; exit 1; }; echo ok; exit 0 ;;
  esac
done
EOF
  chmod +x "$1/netmon"
}

new_root() {
  ROOT="$WORK/root-$1" STATE="$WORK/state-$1" P="$WORK/payload-$1"
  rm -rf "$ROOT" "$STATE" "$P"
  mkdir -p "$ROOT/etc/systemd/system" "$ROOT/usr/local/bin" "$ROOT/proc/sys/net/ipv4" "$STATE"
  printf 'ID=debian\nVERSION_ID="13"\n' > "$ROOT/etc/os-release"
  printf '0\t2147483647\n' > "$ROOT/proc/sys/net/ipv4/ping_group_range"
  touch "$STATE/svc-mosquitto-active"
  : > "$STATE/calls"
  make_payload "$P" v1
}
run() {
  local envs=(); while [ "$1" != -- ]; do envs+=("$1"); shift; done; shift
  env ${envs[@]+"${envs[@]}"} ROOT="$ROOT" STATE="$STATE" CONFIRM_TTY="$STATE/answers" \
    PATH="$SHIMS:$PATH" "$BASH_BIN" "$P/deploy/remote.sh" "$@" > "$STATE/out" 2>&1
}
answer() { echo "$1" > "$STATE/answers"; }
out_has() { grep -qF -- "$1" "$STATE/out"; }
called() { grep -qF -- "$1" "$STATE/calls"; }

echo "1. --check on a Pi without netmon"
new_root check
run -- --check install && pass "exits 0" || fail "failed: $(cat "$STATE/out")"
for p in "install /usr/local/bin/netmon (v1, now: none)" "install /etc/netmon/config.toml" "install /etc/systemd/system/netmon.service" "enable + (re)start netmon"; do
  check "plan: $p" out_has "$p"
done
check "nothing installed" not test -e "$ROOT/usr/local/bin/netmon"
check "no service touched" not grep -qE 'systemctl (restart|enable)' "$STATE/calls"

echo "2. install"
new_root inst; answer y
run -- install && pass "exits 0" || fail "failed: $(cat "$STATE/out")"
check "binary installed" bash -c "[ \"\$('$ROOT/usr/local/bin/netmon' -version)\" = v1 ]"
check "config installed from the example" cmp -s "$SRC/config.toml.example" "$ROOT/etc/netmon/config.toml"
check "unit installed" cmp -s "$SRC/deploy/netmon.service" "$ROOT/etc/systemd/system/netmon.service"
check "daemon-reload, enable, restart" bash -c "grep -q 'daemon-reload' '$STATE/calls' && grep -q 'enable netmon' '$STATE/calls' && grep -q 'restart netmon' '$STATE/calls'"
check "verified on garibaldi" out_has "garibaldi received a live sites/schoolhouse/network/state via the bridge: status ok"

echo "3. re-run: nothing to do"
: > "$STATE/calls"
run -- install && pass "exits 0" || fail "failed: $(cat "$STATE/out")"
check "nothing to do" out_has "nothing to do: netmon v1 installed and running"
check "no restart" not called "restart"

echo "4. upgrade keeps an edited config"
echo "# edited on the Pi" >> "$ROOT/etc/netmon/config.toml"
make_payload "$P" v2; answer y; : > "$STATE/calls"
run -- --check install || true
check "upgrade plan: new binary and a restart" bash -c "grep -qF 'install /usr/local/bin/netmon (v2, now: v1)' '$STATE/out' && grep -qF 'enable + (re)start netmon' '$STATE/out'"
run -- install && pass "exits 0" || fail "failed: $(cat "$STATE/out")"
check "binary upgraded" bash -c "[ \"\$('$ROOT/usr/local/bin/netmon' -version)\" = v2 ]"
check "edited config kept" grep -q "edited on the Pi" "$ROOT/etc/netmon/config.toml"
check "old binary backed up" bash -c "ls '$ROOT'/var/backups/netmon/*/usr/local/bin/netmon >/dev/null"
check "restarted" called "restart netmon"

echo "5. failures restore"
for mode in journal garibaldi start config; do
  new_root "fresh-$mode"; answer y
  if run FAIL_VERIFY="$mode" -- install; then fail "$mode (fresh): should fail"; continue; fi
  check "$mode (fresh): binary and unit removed" bash -c "[ ! -e '$ROOT/usr/local/bin/netmon' ] && [ ! -e '$ROOT/etc/systemd/system/netmon.service' ]"
  check "$mode (fresh): disabled" not test -e "$STATE/svc-netmon-enabled"
  check "$mode (fresh): config kept" test -f "$ROOT/etc/netmon/config.toml"
done
new_root upg-fail; answer y
run -- install || fail "first install failed"
make_payload "$P" v2
if run FAIL_VERIFY=journal -- install; then fail "failed upgrade: should fail"; fi
check "failed upgrade: v1 back" bash -c "[ \"\$('$ROOT/usr/local/bin/netmon' -version)\" = v1 ]"
check "failed upgrade: restarted on v1" out_has "previous version restored and restarted"

echo "6. preflight refusals"
new_root ping; answer y; printf '1\t0\n' > "$ROOT/proc/sys/net/ipv4/ping_group_range"
run -- install && fail "ping range: should refuse" || { out_has "ping_group_range" && pass "restricted ping_group_range refused"; }
new_root mosq; answer y; rm "$STATE/svc-mosquitto-active"
run -- install && fail "no mosquitto: should refuse" || { out_has "local mosquitto is not running" && pass "no local mosquitto refused"; }
new_root os; answer y; printf 'ID=debian\nVERSION_ID="12"\n' > "$ROOT/etc/os-release"
run -- install && fail "debian 12: should refuse" || { out_has "expected Debian 13" && pass "wrong OS refused"; }
check "refusals installed nothing" bash -c "! ls '$WORK'/root-{ping,mosq,os}/usr/local/bin/netmon 2>/dev/null"

echo "7. prompt"
new_root prompt; answer n
run -- install && fail "answer n: should exit non-zero" || pass "answer n: exits non-zero"
check "answer n: nothing installed" not test -e "$ROOT/usr/local/bin/netmon"
rm -f "$STATE/answers"
run -- install && fail "no tty: should refuse" || { out_has "re-run with --yes" && pass "no tty, no --yes: refuses"; }

echo "8. rollback"
new_root rb; answer y
run -- install || fail "install failed"
run -- --check rollback && out_has "remove /usr/local/bin/netmon" && pass "--check lists the removal" || fail "rollback --check: $(cat "$STATE/out")"
check "--check removed nothing" test -e "$ROOT/usr/local/bin/netmon"
: > "$STATE/calls"
run -- rollback && pass "rollback exits 0" || fail "rollback failed: $(cat "$STATE/out")"
check "binary and unit removed" bash -c "[ ! -e '$ROOT/usr/local/bin/netmon' ] && [ ! -e '$ROOT/etc/systemd/system/netmon.service' ]"
check "config kept" test -f "$ROOT/etc/netmon/config.toml"
check "retained topics cleared" bash -c "for l in state outage last_outage; do grep -q \"mosquitto_pub -h 127.0.0.1 -t sites/schoolhouse/network/\$l -r -n\" '$STATE/calls' || exit 1; done"
run -- rollback && out_has "nothing to do" && pass "second rollback: nothing to do" || fail "second rollback: $(cat "$STATE/out")"

echo
if [ $fails -eq 0 ]; then echo "all passed"; else echo "$fails failed"; exit 1; fi
