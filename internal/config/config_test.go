package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExampleLoads(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "")
	cfg, err := Load("../../config.toml.example")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site.ID != "schoolhouse" || cfg.Probe.Interval != 10*time.Second || cfg.Probe.BrokerAddr != "100.64.0.20:1883" ||
		cfg.MQTT.ClientID != "netmon-schoolhouse" || cfg.StateDir != "/var/lib/netmon" {
		t.Errorf("got %+v", cfg)
	}
	if got := cfg.Topic("last_outage"); got != "sites/schoolhouse/network/last_outage" {
		t.Errorf("topic %q", got)
	}
}

func TestDefaultsAndDerivedClientID(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "/var/lib/netmon")
	cfg, err := Load(write(t, "[site]\nid = \"home\"\n[probe]\nbroker_addr = \"10.0.0.2:1883\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.ClientID != "netmon-home" || cfg.MQTT.Broker != "tcp://127.0.0.1:1883" || len(cfg.Probe.Internet) != 3 || cfg.StateDir != "/var/lib/netmon" {
		t.Errorf("got %+v", cfg)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("NETMON_SITE_ID", "office")
	t.Setenv("NETMON_INTERVAL", "15s")
	t.Setenv("NETMON_INTERNET", "1.1.1.1, 8.8.4.4")
	t.Setenv("NETMON_BROKER_ADDR", "10.0.0.9:1883")
	cfg, err := Load(write(t, "[site]\nid = \"home\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site.ID != "office" || cfg.Probe.Interval != 15*time.Second || strings.Join(cfg.Probe.Internet, ",") != "1.1.1.1,8.8.4.4" || cfg.Probe.BrokerAddr != "10.0.0.9:1883" {
		t.Errorf("got %+v", cfg)
	}
	t.Setenv("NETMON_INTERVAL", "soon")
	if _, err := Load(""); err == nil {
		t.Error("bad NETMON_INTERVAL accepted")
	}
}

func TestRejects(t *testing.T) {
	base := "[site]\nid = \"home\"\n[probe]\nbroker_addr = \"10.0.0.2:1883\"\n"
	cases := map[string]string{
		"unknown key":      base + "bogus = 1\n",
		"no site":          "[probe]\nbroker_addr = \"x:1\"\n",
		"bad site":         strings.Replace(base, "\"home\"", "\"a/b\"", 1),
		"no broker addr":   "[site]\nid = \"home\"\n",
		"rounds too long":  base + "count = 10\ntimeout = \"2s\"\n",
		"zero down_after":  base + "down_after = 0\n",
		"no internet":      base + "internet = []\n",
		"bad toml":         "[site\n",
		"bad duration str": base + "interval = \"often\"\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
