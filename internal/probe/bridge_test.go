package probe

import (
	"context"
	"strings"
	"testing"
)

func TestBridgeWatch(t *testing.T) {
	b := &BridgeWatch{Topic: "sites/schoolhouse/bridge"}
	if r := b.Result(); r.OK || !strings.Contains(r.Err, "unknown") || r.Target != "sites/schoolhouse/bridge" {
		t.Errorf("before any message: %+v", r)
	}
	b.Set("1")
	if r := b.Result(); !r.OK || r.Err != "" {
		t.Errorf("1: %+v", r)
	}
	b.Set("0")
	if r := b.Result(); r.OK || !strings.Contains(r.Err, "down") {
		t.Errorf("0: %+v", r)
	}
	b.Set(" 1\n")
	if r := b.Result(); !r.OK {
		t.Errorf("1 with whitespace: %+v", r)
	}
	b.Set("yes")
	if r := b.Result(); r.OK || !strings.Contains(r.Err, `"yes"`) {
		t.Errorf("garbage: %+v", r)
	}
	b.Set("1")
	b.Forget()
	if r := b.Result(); r.OK || !strings.Contains(r.Err, "unknown") {
		t.Errorf("after Forget: %+v", r)
	}
}

func TestRoundBridge(t *testing.T) {
	cfg := Config{Internet: []string{"i1"}, Count: 1}
	gw := func() (string, error) { return "10.0.0.1", nil }
	s := &System{Cfg: cfg, Ping: fakePing(map[string]PingStats{}), GatewayFn: gw}
	if r := s.Round(context.Background()); r.Bridge != nil {
		t.Errorf("no watch: got %+v", r.Bridge)
	}
	s.Bridge = &BridgeWatch{Topic: "sites/x/bridge"}
	s.Bridge.Set("1")
	if r := s.Round(context.Background()); r.Bridge == nil || !r.Bridge.OK || r.Bridge.Target != "sites/x/bridge" {
		t.Errorf("watched: got %+v", r.Bridge)
	}
}
