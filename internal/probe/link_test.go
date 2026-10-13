package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// As pi-helper writes it on pi3b-schoolhouse.
const piHelperEnv = `NETWORK_GATEWAY=192.168.0.1
NETWORK_HOST=pi3b-schoolhouse
NETWORK_IP=192.168.0.87
NETWORK_STATUS=connected
NETWORK_TYPE=ethernet
NETWORK_WIFI_SSID=
NETWORK_WIFI_STATUS=disconnected
`

func TestParseLink(t *testing.T) {
	l, err := parseLink(strings.NewReader(piHelperEnv + "# comment\nGARBAGE\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Link{Status: "connected", Type: "ethernet", IP: "192.168.0.87", WifiStatus: "disconnected"}
	if *l != want {
		t.Errorf("got %+v, want %+v", *l, want)
	}
}

func TestReadLink(t *testing.T) {
	dir := t.TempDir()
	if l, err := ReadLink(filepath.Join(dir, "absent.env")); l != nil || err != nil {
		t.Errorf("absent file: got %+v, %v; want nil, nil", l, err)
	}
	p := filepath.Join(dir, "pi-helper.env")
	if err := os.WriteFile(p, []byte("NETWORK_TYPE=wifi\nNETWORK_WIFI_SSID=School Net\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := ReadLink(p)
	if err != nil || l == nil || l.Type != "wifi" || l.WifiSSID != "School Net" {
		t.Errorf("got %+v, %v", l, err)
	}
}

func TestRoundLink(t *testing.T) {
	base := Config{Internet: []string{"i1"}, Count: 1, Timeout: 100 * time.Millisecond}
	ping := fakePing(map[string]PingStats{})
	gw := func() (string, error) { return "10.0.0.1", nil }
	var asked string
	ok := func(path string) (*Link, error) { asked = path; return &Link{Type: "ethernet"}, nil }

	cfg := base
	cfg.PiHelperEnv = "/run/pi-helper.env"
	r := (&System{Cfg: cfg, Ping: ping, GatewayFn: gw, LinkFn: ok}).Round(context.Background())
	if r.Link == nil || r.Link.Type != "ethernet" || asked != "/run/pi-helper.env" {
		t.Errorf("configured: got %+v (asked %q)", r.Link, asked)
	}

	asked = ""
	r = (&System{Cfg: base, Ping: ping, GatewayFn: gw, LinkFn: ok}).Round(context.Background())
	if r.Link != nil || asked != "" {
		t.Errorf("not configured: got %+v, read %q", r.Link, asked)
	}

	bad := func(string) (*Link, error) { return &Link{Type: "x"}, errors.New("permission denied") }
	r = (&System{Cfg: cfg, Ping: ping, GatewayFn: gw, LinkFn: bad}).Round(context.Background())
	if r.Link != nil {
		t.Errorf("unreadable: got %+v, want nil", r.Link)
	}
}
