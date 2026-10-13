package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sweeney/netmon/internal/config"
	"github.com/sweeney/netmon/internal/outages"
	"github.com/sweeney/netmon/internal/probe"
	"github.com/sweeney/netmon/internal/publisher"
)

// script is a Prober that plays back one round per Step, advancing the clock
// by the interval each time, as the real loop would.
type script struct {
	clock  *outages.FakeClock
	rounds []probe.Round
	i      int
}

func (s *script) Round(context.Context) probe.Round {
	s.clock.Advance(10 * time.Second)
	r := s.rounds[s.i]
	if s.i < len(s.rounds)-1 {
		s.i++
	}
	return r
}

func f(v float64) *float64 { return &v }

func bridge(ok bool) *probe.Result {
	r := probe.Result{OK: ok, Target: "sites/schoolhouse/bridge"}
	return &r
}

func res(ok bool) probe.Result {
	if ok {
		return probe.Result{OK: true, RTTms: f(5), LossPct: f(0)}
	}
	return probe.Result{Err: "no reply"}
}

var (
	allOK  = probe.Round{Gateway: res(true), Internet: res(true), DNS: res(true), Bridge: bridge(true)}
	wan    = probe.Round{Gateway: res(true), Internet: res(false), DNS: res(false), Bridge: bridge(false)}
	lan    = probe.Round{Gateway: res(false), Internet: res(false), DNS: res(false), Bridge: bridge(false)}
	t0     = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	topicS = "sites/schoolhouse/network/state"
	topicO = "sites/schoolhouse/network/outage"
	topicL = "sites/schoolhouse/network/last_outage"
)

func rounds(parts ...any) []probe.Round {
	var out []probe.Round
	for i := 0; i < len(parts); i += 2 {
		for n := 0; n < parts[i+1].(int); n++ {
			out = append(out, parts[i].(probe.Round))
		}
	}
	return out
}

func testCfg() config.Config {
	cfg := config.Defaults()
	cfg.Site.ID = "schoolhouse"
	cfg.Probe.BridgeTopic = "sites/schoolhouse/bridge"
	return cfg
}

type rig struct {
	d     *Daemon
	clock *outages.FakeClock
	pub   *publisher.Fake
	store *outages.MemoryStore
}

func newRig(rs []probe.Round, pub *publisher.Fake, store *outages.MemoryStore, clock *outages.FakeClock) *rig {
	if clock == nil {
		b := -time.Hour
		clock = &outages.FakeClock{Wall: t0, Boot: &b, IsSynced: true, SyncKnown: true, BootIDValue: "boot-1"}
	}
	pr := &script{clock: clock, rounds: rs}
	tr := outages.NewTracker(clock, store)
	return &rig{d: New(testCfg(), pr, pub, clock, tr), clock: clock, pub: pub, store: store}
}

func (r *rig) steps(n int) {
	for i := 0; i < n; i++ {
		r.d.Step(context.Background())
	}
}

func decode(t *testing.T, m publisher.Message) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(m.Payload), &v); err != nil {
		t.Fatalf("%s: %v: %q", m.Topic, err, m.Payload)
	}
	return v
}

func TestOutageEndToEnd(t *testing.T) {
	pub := &publisher.Fake{}
	r := newRig(rounds(allOK, 3, wan, 4, lan, 3, allOK, 3), pub, &outages.MemoryStore{}, nil)

	r.steps(3)
	st, _ := pub.Retained(topicS)
	if s := decode(t, st); s["status"] != "ok" || s["online"] != true {
		t.Fatalf("state: %v", s)
	}
	if st.QOS != StateQOS {
		t.Errorf("state QoS %d", st.QOS)
	}
	if _, ok := pub.Retained(topicO); ok {
		t.Error("an outage is retained while all is well")
	}
	if len(pub.All(topicO)) != 1 || pub.All(topicO)[0].Payload != "" {
		t.Error("the startup clear of a stale outage was not published once")
	}

	r.steps(2) // two wan rounds: not yet
	if s := decode(t, mustRetained(t, pub, topicS)); s["status"] != "ok" || s["raw_status"] != "wan_down" {
		t.Errorf("before down_after: %v", s)
	}
	r.steps(1) // third: outage
	o := mustRetained(t, pub, topicO)
	if o.QOS != EventQOS {
		t.Errorf("outage QoS %d", o.QOS)
	}
	if v := decode(t, o); v["status"] != "wan_down" {
		t.Errorf("outage: %v", v)
	}
	r.steps(1 + 3) // one more wan, then lan escalates after 3
	if v := decode(t, mustRetained(t, pub, topicO)); v["status"] != "lan_down" || v["worst_status"] != "lan_down" {
		t.Errorf("escalated: %v", v)
	}
	r.steps(2) // recovers after up_after
	if _, ok := pub.Retained(topicO); ok {
		t.Error("outage still retained after recovery")
	}
	l := mustRetained(t, pub, topicL)
	v := decode(t, l)
	if l.QOS != EventQOS || v["worst_status"] != "lan_down" || v["spans_restart"] != false {
		t.Errorf("last_outage: %v", v)
	}
	// wan from round 4 (dated by its start, before the clock moved: 30 s),
	// ok from round 11 (100 s): 70 s.
	if v["duration_secs"].(float64) != 70 {
		t.Errorf("duration %v", v["duration_secs"])
	}
	if tr := v["transitions"].([]any); len(tr) != 3 {
		t.Errorf("transitions: %v", tr)
	}
	// Order on the wire: the clear and the summary before the state that says ok.
	iClear, iSum, iState := -1, -1, -1
	for i, m := range pub.Messages {
		switch {
		case m.Topic == topicO && m.Payload == "" && i > 3:
			iClear = i
		case m.Topic == topicL:
			iSum = i
		case m.Topic == topicS && iSum >= 0 && iState < 0:
			iState = i
		}
	}
	if !(iClear < iSum && iSum < iState) {
		t.Errorf("order: clear %d, summary %d, state %d", iClear, iSum, iState)
	}
	if r.store.State() != nil {
		t.Error("state file left after the summary went out")
	}
}

func mustRetained(t *testing.T, pub *publisher.Fake, topic string) publisher.Message {
	t.Helper()
	m, ok := pub.Retained(topic)
	if !ok {
		t.Fatalf("nothing retained on %s", topic)
	}
	return m
}

func TestRestartMidOutage(t *testing.T) {
	pub, store := &publisher.Fake{}, &outages.MemoryStore{}
	r := newRig(rounds(allOK, 2, wan, 5), pub, store, nil)
	r.steps(7)
	if _, ok := pub.Retained(topicO); !ok {
		t.Fatal("no outage before the restart")
	}
	// Process restart in the same boot; the network is still down at first.
	c2 := *r.clock
	c2.M = 0
	b := *r.clock.Boot - r.clock.M
	c2.Boot = &b
	r2 := newRig(rounds(wan, 3, allOK, 2), pub, store, &c2)
	r2.steps(3)
	o := decode(t, mustRetained(t, pub, topicO))
	if o["started_at"] != "2026-10-13T10:00:20Z" {
		t.Errorf("resumed start: %v", o["started_at"])
	}
	r2.steps(2)
	v := decode(t, mustRetained(t, pub, topicL))
	if v["spans_restart"] != true || v["started_at"] != "2026-10-13T10:00:20Z" {
		t.Errorf("last_outage: %v", v)
	}
	if _, ok := pub.Retained(topicO); ok {
		t.Error("outage still retained")
	}
}

func TestRestartAfterOutageEndedWhileDown(t *testing.T) {
	pub, store := &publisher.Fake{}, &outages.MemoryStore{}
	r := newRig(rounds(allOK, 2, lan, 3), pub, store, nil)
	r.steps(5) // outage open, retained
	// The Pi loses power and reboots on mains: the network is fine now.
	b := -40 * time.Second
	c2 := &outages.FakeClock{Wall: r.clock.Wall.Add(5 * time.Minute), Boot: &b, IsSynced: true, SyncKnown: true, BootIDValue: "boot-2"}
	r2 := newRig(rounds(allOK, 3), pub, store, c2)
	r2.steps(2)
	if _, ok := pub.Retained(topicO); ok {
		t.Error("stale outage from before the reboot still retained")
	}
	v := decode(t, mustRetained(t, pub, topicL))
	// The new process starts at 10:05:50, 40 s after boot.
	if v["spans_restart"] != true || v["ended_at"] != "2026-10-13T10:05:10Z" {
		t.Errorf("last_outage: %v", v)
	}
}

func TestPublishFailureThenReconnect(t *testing.T) {
	pub := &publisher.Fake{}
	r := newRig(rounds(allOK, 2, wan, 3, wan, 1), pub, &outages.MemoryStore{}, nil)
	r.steps(2)
	pub.Fail = true
	r.steps(3) // the outage starts while the broker is unreachable
	if _, ok := pub.Retained(topicO); ok {
		t.Fatal("published while failing")
	}
	pub.Fail = false
	r.steps(1) // the next round retries the outage by itself
	if _, ok := pub.Retained(topicO); !ok {
		t.Fatal("outage not retried after the failure")
	}
	n := len(pub.All(topicO))
	r.d.Reconnected()
	r.steps(1)
	if len(pub.All(topicO)) != n+1 {
		t.Error("outage not republished after a reconnect")
	}
}

func TestOffline(t *testing.T) {
	pub := &publisher.Fake{}
	r := newRig(rounds(allOK, 1), pub, &outages.MemoryStore{}, nil)
	if !r.d.Offline() {
		t.Fatal("offline failed")
	}
	m := mustRetained(t, pub, topicS)
	if v := decode(t, m); v["online"] != false || v["timestamp"] == nil || m.QOS != EventQOS {
		t.Errorf("offline: %v QoS %d", v, m.QOS)
	}
	var will map[string]any
	if err := json.Unmarshal([]byte(WillPayload()), &will); err != nil || will["online"] != false {
		t.Errorf("will: %q", WillPayload())
	}
}

func TestStatePayloadShape(t *testing.T) {
	pub := &publisher.Fake{}
	r := newRig(rounds(allOK, 1), pub, &outages.MemoryStore{}, nil)
	r.steps(1)
	v := decode(t, mustRetained(t, pub, topicS))
	if v["status"] != "unknown" {
		t.Errorf("first round: %v", v["status"])
	}
	if _, has := v["status_since"]; has {
		t.Error("status_since before any status settled")
	}
	checks := v["checks"].(map[string]any)
	for _, k := range []string{"gateway", "internet", "dns", "bridge"} {
		if _, ok := checks[k]; !ok {
			t.Errorf("missing check %s", k)
		}
	}
	if _, has := checks["gateway"].(map[string]any)["targets"]; has {
		t.Error("targets on a single-host check")
	}
	if _, has := v["tailscale"]; has {
		t.Error("tailscale present though unknown")
	}
}
