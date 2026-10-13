#!/usr/bin/env bash
# Smoke test: the real binary against a real Mosquitto (Homebrew's), with
# every probe aimed at this machine so the network is "ok". Checks the state
# arrives retained, the status settles, and a SIGTERM leaves online:false.
# About 40 s.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
MOSQUITTO="${MOSQUITTO:-$(command -v mosquitto || echo /opt/homebrew/sbin/mosquitto)}"
for b in "$MOSQUITTO" mosquitto_sub go; do command -v "$b" >/dev/null || { echo "missing: $b" >&2; exit 2; }; done
PORT=21893
if nc -z 127.0.0.1 $PORT 2>/dev/null; then echo "port $PORT in use" >&2; exit 2; fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/netmon-smoke.XXXXXX")"
BROKER_PID="" NETMON_PID="" fails=0
cleanup() {
  [ -n "$NETMON_PID" ] && kill "$NETMON_PID" 2>/dev/null || true
  [ -n "$BROKER_PID" ] && kill "$BROKER_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  if [ "$fails" -gt 0 ] || [ "${KEEP:-}" = 1 ]; then echo "logs kept in $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT
pass() { echo "  PASS  $*"; }
fail() { echo "  FAIL  $*"; fails=$((fails + 1)); }

printf 'listener %s 127.0.0.1\nallow_anonymous true\n' $PORT > "$WORK/mosquitto.conf"
"$MOSQUITTO" -c "$WORK/mosquitto.conf" > "$WORK/mosquitto.log" 2>&1 & BROKER_PID=$!
for _ in $(seq 50); do nc -z 127.0.0.1 $PORT 2>/dev/null && break; sleep 0.1; done

cat > "$WORK/config.toml" <<CFG
state_dir = "$WORK/state"
[site]
id = "smoketest"
[probe]
interval = "3s"
timeout = "500ms"
count = 2
gateway = "127.0.0.1"
internet = ["127.0.0.1"]
dns_server = "127.0.0.1:1"
dns_name = "example.com"
broker_addr = "127.0.0.1:$PORT"
[mqtt]
broker = "tcp://127.0.0.1:$PORT"
CFG
mkdir -p "$WORK/state"
(cd "$HERE/.." && go build -o "$WORK/netmon" ./cmd/netmon)
"$WORK/netmon" -config "$WORK/config.toml" > "$WORK/netmon.log" 2>&1 & NETMON_PID=$!

T=sites/smoketest/network
state() { mosquitto_sub -p $PORT -t "$T/state" --retained-only -C 1 -W 2 2>/dev/null || true; }
got=""
for _ in $(seq 20); do got="$(state)"; [[ "$got" == *'"status":"degraded"'* ]] && break; sleep 1; done
# DNS points at a closed port, so everything else ok -> degraded.
[[ "$got" == *'"status":"degraded"'* ]] && pass "status settled: degraded (DNS aimed at a closed port)" || fail "status never settled: $got"
[[ "$got" == *'"online":true'* ]] && pass "online:true, retained" || fail "no online:true"
[[ "$got" == *'"gateway":{"ok":true'* ]] && pass "gateway check ok (real ICMP to 127.0.0.1)" || fail "gateway check: $got"
grep -q "connected to MQTT broker" "$WORK/netmon.log" && pass "logged its connect" || fail "no connect line"

kill -TERM "$NETMON_PID"; wait "$NETMON_PID" 2>/dev/null || true; NETMON_PID=""
got="$(state)"
[[ "$got" == *'"online":false'* ]] && pass "SIGTERM leaves online:false retained" || fail "after SIGTERM: $got"
[ -z "$(ls -A "$WORK/state")" ] && pass "no outage state written" || fail "state dir: $(ls "$WORK/state")"

echo
if [ $fails -eq 0 ]; then echo "all passed"; else echo "$fails failed"; exit 1; fi
