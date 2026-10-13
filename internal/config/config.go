// Package config loads netmon's TOML configuration, with NETMON_* environment
// overrides for the settings that differ between hosts.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the whole configuration.
type Config struct {
	Site     SiteConfig  `toml:"site"`
	Probe    ProbeConfig `toml:"probe"`
	MQTT     MQTTConfig  `toml:"mqtt"`
	StateDir string      `toml:"state_dir"` // default $STATE_DIRECTORY; empty: no persistence
}

// SiteConfig names the site, as in the swee.net "sites" config.
type SiteConfig struct {
	ID string `toml:"id"`
}

// ProbeConfig says what to check and how to judge it.
type ProbeConfig struct {
	Interval        time.Duration `toml:"interval"`
	DownAfter       int           `toml:"down_after"`
	UpAfter         int           `toml:"up_after"`
	Count           int           `toml:"count"`
	Timeout         time.Duration `toml:"timeout"`
	Gateway         string        `toml:"gateway"` // empty: the default route's
	Internet        []string      `toml:"internet"`
	DNSServer       string        `toml:"dns_server"`
	DNSName         string        `toml:"dns_name"`
	BrokerAddr      string        `toml:"broker_addr"`
	TailscalePeer   string        `toml:"tailscale_peer"`
	PiHelperEnv     string        `toml:"pi_helper_env"` // empty: don't read it
	DegradedLossPct float64       `toml:"degraded_loss_pct"`
	DegradedRTTms   float64       `toml:"degraded_rtt_ms"`
}

// MQTTConfig is where to publish.
type MQTTConfig struct {
	Broker      string `toml:"broker"`
	ClientID    string `toml:"client_id"`
	TopicPrefix string `toml:"topic_prefix"`
	Username    string `toml:"username"`
	Password    string `toml:"password"`
}

// Defaults returns the configuration used for anything a file leaves out.
func Defaults() Config {
	return Config{
		Probe: ProbeConfig{
			Interval:        10 * time.Second,
			DownAfter:       3,
			UpAfter:         2,
			Count:           3,
			Timeout:         time.Second,
			Internet:        []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"},
			DNSServer:       "1.1.1.1:53",
			DNSName:         "example.com",
			PiHelperEnv:     "/run/pi-helper.env",
			DegradedLossPct: 20,
			DegradedRTTms:   500,
		},
		MQTT: MQTTConfig{
			Broker:      "tcp://127.0.0.1:1883",
			TopicPrefix: "sites",
		},
		StateDir: os.Getenv("STATE_DIRECTORY"),
	}
}

// Load reads path over the defaults, applies environment overrides and
// validates the result.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		md, err := toml.DecodeFile(path, &cfg)
		if err != nil {
			return cfg, fmt.Errorf("reading %s: %w", path, err)
		}
		if un := md.Undecoded(); len(un) > 0 {
			return cfg, fmt.Errorf("%s: unknown settings: %v", path, un)
		}
	}
	if err := applyEnv(&cfg, os.Getenv); err != nil {
		return cfg, err
	}
	if cfg.MQTT.ClientID == "" && cfg.Site.ID != "" {
		cfg.MQTT.ClientID = "netmon-" + cfg.Site.ID
	}
	return cfg, cfg.Validate()
}

func applyEnv(cfg *Config, get func(string) string) error {
	str := map[string]*string{
		"NETMON_SITE_ID":        &cfg.Site.ID,
		"NETMON_MQTT_BROKER":    &cfg.MQTT.Broker,
		"NETMON_MQTT_CLIENT_ID": &cfg.MQTT.ClientID,
		"NETMON_MQTT_USERNAME":  &cfg.MQTT.Username,
		"NETMON_MQTT_PASSWORD":  &cfg.MQTT.Password,
		"NETMON_BROKER_ADDR":    &cfg.Probe.BrokerAddr,
		"NETMON_TAILSCALE_PEER": &cfg.Probe.TailscalePeer,
		"NETMON_STATE_DIR":      &cfg.StateDir,
	}
	for k, p := range str {
		if v := get(k); v != "" {
			*p = v
		}
	}
	if v := get("NETMON_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("NETMON_INTERVAL: %w", err)
		}
		cfg.Probe.Interval = d
	}
	if v := get("NETMON_INTERNET"); v != "" {
		cfg.Probe.Internet = strings.Fields(strings.ReplaceAll(v, ",", " "))
	}
	if v := get("NETMON_DOWN_AFTER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("NETMON_DOWN_AFTER: %w", err)
		}
		cfg.Probe.DownAfter = n
	}
	return nil
}

// Validate reports the first setting that cannot work.
func (c Config) Validate() error {
	p := c.Probe
	switch {
	case c.Site.ID == "":
		return fmt.Errorf("site.id is required (e.g. \"schoolhouse\")")
	case strings.ContainsAny(c.Site.ID, "/#+ "):
		return fmt.Errorf("site.id %q must not contain / # + or spaces", c.Site.ID)
	case p.Interval < time.Second:
		return fmt.Errorf("probe.interval %s is too short", p.Interval)
	case p.DownAfter < 1 || p.UpAfter < 1:
		return fmt.Errorf("probe.down_after and probe.up_after must be at least 1")
	case p.Count < 1:
		return fmt.Errorf("probe.count must be at least 1")
	case p.Timeout <= 0:
		return fmt.Errorf("probe.timeout must be positive")
	case time.Duration(p.Count)*(p.Timeout+100*time.Millisecond) >= p.Interval:
		return fmt.Errorf("probe.count × probe.timeout (%d × %s) must fit inside probe.interval (%s)", p.Count, p.Timeout, p.Interval)
	case len(p.Internet) == 0:
		return fmt.Errorf("probe.internet needs at least one host")
	case p.BrokerAddr == "":
		return fmt.Errorf("probe.broker_addr is required (host:port of the MQTT broker the bridge uses)")
	case c.MQTT.Broker == "":
		return fmt.Errorf("mqtt.broker is required")
	case c.MQTT.TopicPrefix == "":
		return fmt.Errorf("mqtt.topic_prefix is required")
	}
	return nil
}

// Topic is <prefix>/<site>/network/<leaf>.
func (c Config) Topic(leaf string) string {
	return c.MQTT.TopicPrefix + "/" + c.Site.ID + "/network/" + leaf
}
