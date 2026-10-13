package payload

import (
	"strings"
	"testing"
	"time"

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
