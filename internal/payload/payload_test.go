package payload

import (
	"strings"
	"testing"
	"time"

	"github.com/sweeney/netmon/internal/outages"
	"github.com/sweeney/netmon/internal/probe"
)

func TestStateLink(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	r := probe.Round{Link: &probe.Link{Status: "connected", Type: "ethernet", IP: "192.168.0.87", WifiStatus: "disconnected"}}
	got := string(JSON(NewState(now, nil, "ok", "ok", nil, r)))
	want := `"link":{"status":"connected","type":"ethernet","ip":"192.168.0.87","wifi_status":"disconnected"}`
	if !strings.Contains(got, want) {
		t.Errorf("got %s\nwant it to contain %s", got, want)
	}

	got = string(JSON(NewState(now, nil, "ok", "ok", nil, probe.Round{})))
	if strings.Contains(got, `"link"`) {
		t.Errorf("no pi-helper: link should be absent, got %s", got)
	}
}

func TestStateBridge(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	b := probe.Result{Target: "sites/schoolhouse/bridge", Err: "bridge reports its link down"}
	got := string(JSON(NewState(now, nil, "bridge_down", "bridge_down", nil, probe.Round{Bridge: &b})))
	if want := `"bridge":{"ok":false,"target":"sites/schoolhouse/bridge","error":"bridge reports its link down"}`; !strings.Contains(got, want) {
		t.Errorf("got %s\nwant it to contain %s", got, want)
	}
	if got := string(JSON(NewState(now, nil, "ok", "ok", nil, probe.Round{}))); strings.Contains(got, `"bridge"`) {
		t.Errorf("hub: bridge should be absent, got %s", got)
	}
}

func TestStateUnsettledWithTailscale(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	since := now.Add(-time.Minute)
	on, synced := true, false
	r := probe.Round{Tailscale: &probe.Tailscale{Path: "relay", Relay: "lhr", Online: &on}}
	got := string(JSON(NewState(now, &synced, "", "lan_down", &since, r)))
	for _, want := range []string{
		`"status":"unknown"`, `"raw_status":"lan_down"`, `"clock_synced":false`,
		`"status_since":"2026-10-14T09:59:00Z"`, `"tailscale":{"path":"relay","relay":"lhr","online":true}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("got %s\nwant it to contain %s", got, want)
		}
	}
}

func TestOutagePayloads(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	tr := []outages.TransitionOut{{At: t0, Status: "wan_down"}, {At: t0.Add(90 * time.Second), Status: "ok"}}
	o := string(JSON(NewOutage(outages.Open{StartedAt: t0, Status: "wan_down", WorstStatus: "wan_down", Transitions: tr[:1]})))
	if want := `{"started_at":"2026-10-14T09:00:00Z","status":"wan_down","worst_status":"wan_down","transitions":[{"at":"2026-10-14T09:00:00Z","status":"wan_down"}]}`; o != want {
		t.Errorf("outage:\n got %s\nwant %s", o, want)
	}
	l := NewLastOutage(outages.Summary{StartedAt: t0, EndedAt: t0.Add(90*time.Second + 400*time.Millisecond), WorstStatus: "wan_down", Transitions: tr, SpansRestart: true})
	if l.DurationSecs != 90 || !l.SpansRestart || len(l.Transitions) != 2 {
		t.Errorf("last_outage: %+v", l)
	}
}

func TestJSONPanicsOnAnUnmarshalableValue(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	JSON(make(chan int))
}
