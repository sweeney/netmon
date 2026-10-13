# netmon

Watches a site's network from its Pi and publishes what it sees to MQTT.

When the schoolhouse goes dark, garibaldi sees only the bridge's
`sites/schoolhouse/bridge = 0`. netmon says why, from the Pi's side. The
answer goes out live while the link is up, and as a summary that the Pi's
bridge queues and delivers once the link is back.

Status: **deployed** on pi3b-schoolhouse since 2026-10-14. Upgrades need sudo on the Pi.
It also runs on garibaldi as site `home` (below), watching home's own network
and its path to the schoolhouse.
The design is in `pi-setup/PLAN.md`, and how it fits with the bridge and
ups-mqtt is in `pi-setup/ARCHITECTURE.md`.

## What it checks

Every `interval` (10 s), in parallel:

| Check | Target | How |
|---|---|---|
| `gateway` | the default route's gateway, read each round | ICMP echo ×3 |
| `internet` | `1.1.1.1`, `8.8.8.8`, `9.9.9.9`; down only if **all** fail | ICMP echo ×3 each |
| `dns` | `example.com` via `1.1.1.1:53` directly, bypassing MagicDNS | UDP DNS |
| `bridge` | `sites/schoolhouse/bridge` on the local broker | the bridge's own retained status, `1` or `0` |
| `tailscale` | `tailscale status --json`, peer `garibaldi` | direct or relayed, endpoint, last handshake |

**The bridge check** reads the status Mosquitto's bridge publishes, retained,
on its notification topic. netmon subscribes over the connection it already
holds to its local broker, and resubscribes on every reconnect. So it opens no
connections of its own and leaves nothing in either broker's log. (Until
2026-10-14 it was a TCP connect to garibaldi:1883 every round, which logged
two lines on garibaldi each time.) The status is unknown, and the check
fails, until the first message arrives and whenever the local connection is
down.

It follows the bridge, so it lags the link: the bridge notices a silent
failure only when its keepalive lapses (about 45 s), and reconnects with a
5–60 s backoff. Outage ends can be up to a minute late. A host with no
`bridge_topic` (garibaldi, the hub) has no bridge check at all.

`state` also carries `link`: the Pi's link status, type (ethernet or wifi), IP
and wifi status/SSID, read each round from pi-helper's `/run/pi-helper.env`
(github.com/sweeney/pi-helper). Like `tailscale`, it is reported only and does
not affect the status. If the file is absent, `link` is left out; set
`pi_helper_env = ""` to skip it.

ICMP uses unprivileged ping sockets, which Debian's `net.ipv4.ping_group_range`
allows for every group, so the service needs no capabilities.

## Status

Each round gets one raw status, checking from the Pi outwards:

1. `gateway` failing → `lan_down`
2. `internet` failing → `wan_down`
3. `bridge` failing → `bridge_down` (the internet works, but the bridge to the main broker does not: Tailscale, or garibaldi's broker)
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

An **outage** is any time spent in `lan_down`, `wan_down` or `bridge_down`.
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
           "bridge":{"ok":true,"target":"sites/schoolhouse/bridge"}},
 "tailscale":{"path":"direct","endpoint":"203.0.113.10:51826","relay":"lhr",
              "online":true,"last_handshake":"2026-10-13T20:40:10Z"},
 "link":{"status":"connected","type":"ethernet","ip":"192.168.0.87",
         "wifi_status":"disconnected"}}

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
  out when they are published, so a clock that syncs later corrects them. They
  are rounded to the second, so a fixed time (`status_since`, an outage's
  transitions) is identical in every payload; only `timestamp` is precise.
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

Each site's config is in `configs/<site>.toml`. These environment overrides
are supported: `NETMON_SITE_ID`, `NETMON_MQTT_BROKER`, `NETMON_MQTT_CLIENT_ID`,
`NETMON_MQTT_USERNAME`, `NETMON_MQTT_PASSWORD`, `NETMON_BRIDGE_TOPIC`,
`NETMON_TAILSCALE_PEER`, `NETMON_STATE_DIR`, `NETMON_INTERVAL`,
`NETMON_INTERNET` and `NETMON_DOWN_AFTER`.

## Deploy

First, once: `cp deploy/sites.example deploy/sites` and fill in your hosts.
`deploy/sites` is gitignored, so the addresses stay out of the repo.

From this directory:

```
deploy/deploy.sh --check               # read-only, no sudo: what would change
deploy/deploy.sh                       # build, install, verify; asks first
deploy/deploy.sh rollback              # stop and remove (config and state kept)
deploy/deploy.sh --site home --check   # the same for another site
```

`--site` picks a row of `deploy/sites` and its `configs/<site>.toml`; the
default is `schoolhouse`. Each row gives the site id, its ssh target, and its
hub: the main broker the site's bridge feeds, where the install is verified.
`--host` and `--hub` override them. The binary is built for whatever `uname -m` the host reports (arm64
or amd64).

| Site | Host | Bridge check | Tailscale peer | pi-helper |
|---|---|---|---|---|
| `schoolhouse` | pi3b-schoolhouse (arm64) | `sites/schoolhouse/bridge` | `garibaldi` | yes |
| `home` | garibaldi (amd64) | none: garibaldi is the hub | `pi3b-schoolhouse` (reported, not judged) | no |

On garibaldi the status is home's own network: gateway, internet, DNS. The
schoolhouse's link is deliberately not part of it, or every schoolhouse ISP
cut would be recorded as a home outage. A consumer compares the two: home
`ok` while `sites/schoolhouse/bridge` is `0` puts the fault at the schoolhouse
end; home `wan_down` at the same time explains the bridge going down from
garibaldi's side.

It installs `/usr/local/bin/netmon`, `/etc/netmon/config.toml` (only if there
isn't one, so edits on the host survive upgrades) and `netmon.service`
(`DynamicUser`, `StateDirectory=netmon`, `TimeoutStopSec=20s`). It needs a
local Mosquitto: on a site's Pi, the one from `pi-setup/mqtt-bridge`. It
refuses if the host's config is for another site than the one deployed.
A config that is byte-for-byte one netmon shipped before
(`deploy/shipped-configs.sha256`) was never edited, so an upgrade replaces it,
keeping a backup. An edited config that the new binary cannot load stops the
deploy before anything changes. When a shipped config changes, add its old
hash to that list; `tests/deploy-script.sh` fails until you do.

It then checks three things: that netmon logged its MQTT connect, that it is
still running, and that the hub has a live `state` (through the bridge,
from a Pi). If any check fails, it puts back the previous version, or removes
netmon if this was the first install.

## Tests

```
go test -race ./...          # unit and daemon tests (fake prober, clock and broker)
tests/smoke.sh               # the binary against a real mosquitto (brew install mosquitto)
tests/remote-script.sh       # deploy/remote.sh against a fixture root
tests/deploy-script.sh       # deploy/deploy.sh with ssh and go shimmed: sites, hosts, arch
```

`tests/run-all.sh` runs them all. The probes were also run once on each host,
unprivileged, with `-once`.
