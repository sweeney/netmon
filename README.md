# netmon

Watches a site's network from its Pi and publishes what it sees to MQTT.

When the schoolhouse goes dark, garibaldi sees only the bridge's
`sites/schoolhouse/bridge = 0`. netmon says why, from the Pi's side. The
answer goes out live while the link is up, and as a summary that the Pi's
bridge queues and delivers once the link is back.

Status: **built and tested, not deployed.** Deploying needs sudo on the Pi.
The design is in `pi-setup/PLAN.md`, and how it fits with the bridge and
ups-mqtt is in `pi-setup/ARCHITECTURE.md`.

## What it checks

Every `interval` (10 s), in parallel:

| Check | Target | How |
|---|---|---|
| `gateway` | the default route's gateway, read each round | ICMP echo ×3 |
| `internet` | `1.1.1.1`, `8.8.8.8`, `9.9.9.9`; down only if **all** fail | ICMP echo ×3 each |
| `dns` | `example.com` via `1.1.1.1:53` directly, bypassing MagicDNS | UDP DNS |
| `broker` | `100.64.0.20:1883` (garibaldi) | TCP connect |
| `tailscale` | `tailscale status --json`, peer `garibaldi` | direct or relayed, endpoint, last handshake |

ICMP uses unprivileged ping sockets, which Debian's `net.ipv4.ping_group_range`
allows for every group, so the service needs no capabilities.

## Status

Each round gets one raw status, checking from the Pi outwards:

1. `gateway` failing → `lan_down`
2. `internet` failing → `wan_down`
3. `broker` failing → `tailnet_down`
4. otherwise, loss over 20 % or latency over 500 ms anywhere, or DNS failing,
   → `degraded`
5. otherwise → `ok`

**Hysteresis:** the published status gets worse only after `down_after` (3)
rounds in a row that are worse than it, and better only after `up_after` (2)
rounds in a row that are better. The rounds in a run need not agree. A flaky
gateway during a WAN outage alternates `lan_down` and `wan_down`, and a
recovering link alternates `ok` and `degraded`. The run settles on the worst
status it saw, dated from its first round. The status is `unknown` until it
first settles, about 20 s after start.

An **outage** is any time spent in `lan_down`, `wan_down` or `tailnet_down`.
`degraded` is shown live but is not an outage. If the status changes partway
through an outage (say `wan_down` to `lan_down`), it is still one outage, and
the summary lists every transition and the worst status.

## Topics

All are retained, under `sites/<site.id>/network/`:

| Topic | QoS | When |
|---|---|---|
| `state` | 0 | every round; `{"online":false}` on shutdown, and as the last will |
| `outage` | 1 | while an outage is in progress, on each change; cleared (empty) when it ends |
| `last_outage` | 1 | once each outage ends |

QoS follows the bridge convention: readings at 0 are not queued while the
bridge is down, and events at 1 are. One outage produces a handful of events,
not one per round.

```json
// state
{"timestamp":"2026-10-13T20:42:16Z","clock_synced":true,"online":true,
 "status":"ok","status_since":"2026-10-13T20:30:02Z","raw_status":"ok",
 "checks":{"gateway":{"ok":true,"target":"192.168.0.1","rtt_ms":0.7,"loss_pct":0},
           "internet":{"ok":true,"targets":3,"targets_ok":3,"rtt_ms":18.4,"loss_pct":0},
           "dns":{"ok":true,"target":"1.1.1.1:53","rtt_ms":23.5},
           "broker":{"ok":true,"target":"100.64.0.20:1883","rtt_ms":39}},
 "tailscale":{"path":"direct","endpoint":"203.0.113.10:51826","relay":"lhr",
              "online":true,"last_handshake":"2026-10-13T20:40:10Z"}}

// last_outage
{"started_at":"2026-10-13T10:02:10Z","ended_at":"2026-10-13T10:15:42Z",
 "duration_secs":812,"worst_status":"wan_down",
 "transitions":[{"at":"2026-10-13T10:02:10Z","status":"wan_down"},
                {"at":"2026-10-13T10:15:42Z","status":"ok"}],
 "clock_synced":true,"spans_restart":false}
```

Unknown values are absent, not zero.

## Times, restarts and the missing RTC

This follows ups-mqtt's `internal/outages`; its clock code is copied. A Pi 3B
has no real-time clock, so after a power-loss reboot its wall clock is wrong
until NTP syncs, which needs the network that may be down. So:

- Every transition is anchored on the monotonic clock. Wall times are worked
  out when they are published, so a clock that syncs later corrects them.
- If the clock has not synced when an outage ends, the summary waits up to
  15 minutes. It then goes out marked `clock_synced: false`, and a corrected
  one follows after the sync.
- The open outage, and any ended ones whose summaries have not gone out yet,
  are saved to `$STATE_DIRECTORY/outage.json`, on transitions only, to spare
  the SD card. A new outage starting does not drop an older unpublished
  summary. Up to 10 are queued, and they go out oldest first.
- On restart, an outage still in progress is resumed. One that ended while
  netmon was not running is reported with `spans_restart: true`. Its end is
  boot time if the Pi rebooted, or this process's start if it didn't.

## MQTT connection

This is ups-mqtt's publisher, adapted. paho's auto-reconnect is off, so a
lost connection fails publishes at once instead of piling them up in paho's
memory and replaying them out of order. On reconnect, netmon republishes its
current state. It publishes to the Pi's local broker, `tcp://127.0.0.1:1883`.

## Running it

```
netmon -config /etc/netmon/config.toml                # the service
netmon -config config.toml -once                      # one round, printed; no MQTT
netmon -config config.toml -check-config              # validate
```

`config.toml.example` is the schoolhouse config. These environment overrides
are supported: `NETMON_SITE_ID`, `NETMON_MQTT_BROKER`, `NETMON_MQTT_CLIENT_ID`,
`NETMON_MQTT_USERNAME`, `NETMON_MQTT_PASSWORD`, `NETMON_BROKER_ADDR`,
`NETMON_TAILSCALE_PEER`, `NETMON_STATE_DIR`, `NETMON_INTERVAL`,
`NETMON_INTERNET` and `NETMON_DOWN_AFTER`.

## Deploy

From this directory:

```
deploy/deploy.sh --check     # read-only, no sudo: what would change
deploy/deploy.sh             # build for arm64, install, verify; asks first
deploy/deploy.sh rollback    # stop and remove (config and state kept)
```

It installs `/usr/local/bin/netmon`, `/etc/netmon/config.toml` (only if there
isn't one, so edits on the Pi survive upgrades) and `netmon.service`
(`DynamicUser`, `StateDirectory=netmon`, `TimeoutStopSec=20s`). It needs the
local Mosquitto from `pi-setup/mqtt-bridge`.

It then checks three things: that netmon logged its MQTT connect, that it is
still running, and that garibaldi received `state` through the bridge. If any
check fails, it puts back the previous version, or removes netmon if this was
the first install.

## Tests

```
go test -race ./...          # unit and daemon tests (fake prober, clock and broker)
tests/smoke.sh               # the binary against a real mosquitto (brew install mosquitto)
tests/remote-script.sh       # deploy/remote.sh against a fixture root
```

`tests/run-all.sh` runs all three. The probes were also run once on the Pi,
unprivileged, with `-once`.
