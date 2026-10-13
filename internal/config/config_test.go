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

func TestSiteConfigsLoad(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "")
	cfg, err := Load("../../configs/schoolhouse.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site.ID != "schoolhouse" || cfg.Probe.Interval != 10*time.Second || cfg.Probe.BridgeTopic != "sites/schoolhouse/bridge" ||
		cfg.Probe.TailscalePeer != "garibaldi" || cfg.MQTT.ClientID != "netmon-schoolhouse" || cfg.StateDir != "/var/lib/netmon" ||
		cfg.Probe.PiHelperEnv != "/run/pi-helper.env" {
		t.Errorf("schoolhouse: got %+v", cfg)
	}
	if got := cfg.Topic("last_outage"); got != "sites/schoolhouse/network/last_outage" {
		t.Errorf("topic %q", got)
	}

	// garibaldi is the hub: no bridge of its own to watch, and no pi-helper.
	cfg, err = Load("../../configs/home.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site.ID != "home" || cfg.Probe.BridgeTopic != "" || cfg.Probe.TailscalePeer != "pi3b-schoolhouse" ||
		cfg.Probe.PiHelperEnv != "" || cfg.MQTT.Broker != "tcp://127.0.0.1:1883" || cfg.MQTT.ClientID != "netmon-home" ||
		cfg.StateDir != "/var/lib/netmon" {
		t.Errorf("home: got %+v", cfg)
	}
	if got := cfg.Topic("state"); got != "sites/home/network/state" {
		t.Errorf("topic %q", got)
	}
}

func TestDefaultsAndDerivedClientID(t *testing.T) {
	t.Setenv("STATE_DIRECTORY", "/var/lib/netmon")
	cfg, err := Load(write(t, "[site]\nid = \"home\"\n[probe]\nbridge_topic = \"sites/home/bridge\"\n"))
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
	t.Setenv("NETMON_BRIDGE_TOPIC", "sites/office/bridge")
	cfg, err := Load(write(t, "[site]\nid = \"home\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site.ID != "office" || cfg.Probe.Interval != 15*time.Second || strings.Join(cfg.Probe.Internet, ",") != "1.1.1.1,8.8.4.4" || cfg.Probe.BridgeTopic != "sites/office/bridge" {
		t.Errorf("got %+v", cfg)
	}
	t.Setenv("NETMON_INTERVAL", "soon")
	if _, err := Load(""); err == nil {
		t.Error("bad NETMON_INTERVAL accepted")
	}
}

func TestRejects(t *testing.T) {
	base := "[site]\nid = \"home\"\n[probe]\nbridge_topic = \"sites/home/bridge\"\n"
	cases := map[string]string{
		"unknown key":      base + "bogus = 1\n",
		"no site":          "[probe]\nbridge_topic = \"x\"\n",
		"bad site":         strings.Replace(base, "\"home\"", "\"a/b\"", 1),
		"wildcard bridge":  strings.Replace(base, "sites/home/bridge", "sites/+/bridge", 1),
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

func TestRejectsEachSetting(t *testing.T) {
	base := "[site]\nid = \"home\"\n"
	cases := map[string]string{
		"site with a space":  "[site]\nid = \"my home\"\n",
		"interval too short": base + "[probe]\ninterval = \"500ms\"\n",
		"zero up_after":      base + "[probe]\nup_after = 0\n",
		"zero count":         base + "[probe]\ncount = 0\n",
		"zero timeout":       base + "[probe]\ntimeout = \"0s\"\n",
		"no broker":          base + "[mqtt]\nbroker = \"\"\n",
		"no topic prefix":    base + "[mqtt]\ntopic_prefix = \"\"\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEveryEnvOverride(t *testing.T) {
	env := map[string]string{
		"NETMON_MQTT_BROKER":    "tcp://broker:1883",
		"NETMON_MQTT_CLIENT_ID": "netmon-test",
		"NETMON_MQTT_USERNAME":  "u",
		"NETMON_MQTT_PASSWORD":  "p",
		"NETMON_TAILSCALE_PEER": "hub",
		"NETMON_STATE_DIR":      "/tmp/netmon",
		"NETMON_DOWN_AFTER":     "5",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := Load(write(t, "[site]\nid = \"home\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.MQTT
	if m.Broker != "tcp://broker:1883" || m.ClientID != "netmon-test" || m.Username != "u" || m.Password != "p" ||
		cfg.Probe.TailscalePeer != "hub" || cfg.StateDir != "/tmp/netmon" || cfg.Probe.DownAfter != 5 {
		t.Errorf("got %+v", cfg)
	}
	t.Setenv("NETMON_DOWN_AFTER", "three")
	if _, err := Load(""); err == nil {
		t.Error("bad NETMON_DOWN_AFTER accepted")
	}
}
