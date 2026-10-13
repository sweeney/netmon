#!/usr/bin/env bash
# Smoke test: the real binary against a real Mosquitto (Homebrew's), with
# every probe aimed at this machine so the network is "ok". Checks the state
# arrives retained, the status settles, and a SIGTERM leaves online:false.
# About 40 s.
#
# With GOCOVERDIR set, the binary is built with coverage on and writes its
# counters there (tests/coverage.sh uses this).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
MOSQUITTO="${MOSQUITTO:-$(command -v mosquitto || echo /opt/homebrew/sbin/mosquitto)}"
for b in "$MOSQUITTO" mosquitto_sub go jq; do command -v "$b" >/dev/null || { echo "missing: $b" >&2; exit 2; }; done
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
bridge_topic = "sites/smoketest/bridge"
[mqtt]
broker = "tcp://127.0.0.1:$PORT"
CFG
mkdir -p "$WORK/state"
mosquitto_pub -p $PORT -t sites/smoketest/bridge -r -q 1 -m 1   # retained before netmon starts, as the bridge leaves it
COVER=()
[ -n "${GOCOVERDIR:-}" ] && COVER=(-cover -coverpkg=./...)
(cd "$HERE/.." && go build ${COVER[@]+"${COVER[@]}"} -ldflags "-X main.version=smoke" -o "$WORK/netmon" ./cmd/netmon)

# The one-shot modes, before the daemon starts.
[ "$("$WORK/netmon" -version)" = smoke ] && pass "-version" || fail "-version"
"$WORK/netmon" -config "$WORK/config.toml" -check-config | grep -q "ok (site smoketest" && pass "-check-config" || fail "-check-config"
printf '[site]\nid = "a/b"\n' > "$WORK/bad.toml"
if "$WORK/netmon" -config "$WORK/bad.toml" -check-config > "$WORK/bad.log" 2>&1; then fail "bad config accepted"
else grep -q "site.id" "$WORK/bad.log" && pass "bad config refused" || fail "bad config: $(cat "$WORK/bad.log")"; fi
once="$("$WORK/netmon" -config "$WORK/config.toml" -once 2>"$WORK/once.log")"
[[ "$once" == sites/smoketest/network/state* ]] && pass "-once prints the state topic" || fail "-once: $once"
[ "$(sed 1d <<<"$once" | jq -c .checks.bridge)" = '{"ok":true,"target":"sites/smoketest/bridge"}' ] && pass "-once read the retained bridge status" || fail "-once bridge: $once"

"$WORK/netmon" -config "$WORK/config.toml" > "$WORK/netmon.log" 2>&1 & NETMON_PID=$!

T=sites/smoketest/network
state() { mosquitto_sub -p $PORT -t "$T/state" --retained-only -C 1 -W 2 2>/dev/null || true; }
bridge() { mosquitto_pub -p $PORT -t sites/smoketest/bridge -r -q 1 -m "$1"; }
wait_for() {  # wait_for PATTERN SECONDS: until the retained state matches
  got=""; for _ in $(seq "$2"); do got="$(state)"; [[ "$got" == *$1* ]] && return 0; sleep 1; done; return 1
}
got=""
for _ in $(seq 20); do got="$(state)"; [[ "$got" == *'"status":"degraded"'* ]] && break; sleep 1; done
# DNS points at a closed port, so everything else ok -> degraded.
[[ "$got" == *'"status":"degraded"'* ]] && pass "status settled: degraded (DNS aimed at a closed port)" || fail "status never settled: $got"
[[ "$got" == *'"online":true'* ]] && pass "online:true, retained" || fail "no online:true"
[[ "$got" == *'"gateway":{"ok":true'* ]] && pass "gateway check ok (real ICMP to 127.0.0.1)" || fail "gateway check: $got"
grep -q "connected to MQTT broker" "$WORK/netmon.log" && pass "logged its connect" || fail "no connect line"
[[ "$got" == *'"bridge":{"ok":true,"target":"sites/smoketest/bridge"}'* ]] && pass "bridge check read the retained 1" || fail "bridge check: $got"

# The bridge drops: an outage, then its summary once it is back.
bridge 0
wait_for '"status":"bridge_down"' 20 && pass "bridge 0 -> status bridge_down" || fail "no bridge_down: $got"
o="$(mosquitto_sub -p $PORT -t "$T/outage" --retained-only -C 1 -W 2 2>/dev/null || true)"
[[ "$o" == *'"status":"bridge_down"'* ]] && pass "outage published" || fail "outage: $o"
bridge 1
wait_for '"status":"degraded"' 20 && pass "bridge 1 -> recovered" || fail "no recovery: $got"
l="$(mosquitto_sub -p $PORT -t "$T/last_outage" --retained-only -C 1 -W 2 2>/dev/null || true)"
[[ "$l" == *'"worst_status":"bridge_down"'* ]] && pass "last_outage: worst bridge_down" || fail "last_outage: $l"

# The broker restarts (retained messages lost, no persistence here): netmon
# must resubscribe on reconnect, or the bridge check stays unknown.
kill "$BROKER_PID"; wait "$BROKER_PID" 2>/dev/null || true
wait_for_down() { for _ in $(seq 20); do grep -q "connection to .* lost" "$WORK/netmon.log" && return 0; sleep 0.5; done; return 1; }
wait_for_down && pass "netmon saw the broker go" || fail "no connection-lost line"
"$MOSQUITTO" -c "$WORK/mosquitto.conf" >> "$WORK/mosquitto.log" 2>&1 & BROKER_PID=$!
for _ in $(seq 50); do nc -z 127.0.0.1 $PORT 2>/dev/null && break; sleep 0.1; done
bridge 1
for _ in $(seq 30); do grep -q "reconnected to MQTT broker" "$WORK/netmon.log" && break; sleep 1; done
grep -q "reconnected to MQTT broker" "$WORK/netmon.log" && pass "reconnected" || fail "no reconnect"
wait_for '"bridge":{"ok":true' 20 && pass "resubscribed: bridge ok after the reconnect" || fail "after reconnect: $got"

kill -TERM "$NETMON_PID"; wait "$NETMON_PID" 2>/dev/null || true; NETMON_PID=""
got="$(state)"
[[ "$got" == *'"online":false'* ]] && pass "SIGTERM leaves online:false retained" || fail "after SIGTERM: $got"
[ -z "$(ls -A "$WORK/state")" ] && pass "no outage state written" || fail "state dir: $(ls "$WORK/state")"

echo
if [ $fails -eq 0 ]; then echo "all passed"; else echo "$fails failed"; exit 1; fi
