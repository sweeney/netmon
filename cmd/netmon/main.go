// Command netmon watches the network from a site's Pi and publishes what it
// sees to MQTT: a live state every round, and a summary of each outage once
// it is over (which the Pi's bridge queues and delivers on reconnect).
//
//	netmon -config /etc/netmon/config.toml
//	netmon -config config.toml -once          one round, printed; publishes nothing
//	netmon -config config.toml -check-config  validate the config
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/sweeney/netmon/internal/classify"
	"github.com/sweeney/netmon/internal/config"
	"github.com/sweeney/netmon/internal/daemon"
	"github.com/sweeney/netmon/internal/outages"
	"github.com/sweeney/netmon/internal/payload"
	"github.com/sweeney/netmon/internal/probe"
	"github.com/sweeney/netmon/internal/publisher"
	"github.com/sweeney/netmon/internal/retry"
)

var version = "dev" // set by deploy/build.sh

var startupBackoff = retry.Backoff{Initial: time.Second, Max: 30 * time.Second}

func main() {
	configPath := flag.String("config", "/etc/netmon/config.toml", "path to config file")
	once := flag.Bool("once", false, "run one round, print the state payload and exit (no MQTT)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	checkConfig := flag.Bool("check-config", false, "load and validate the config, then exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if *checkConfig {
		fmt.Printf("%s: ok (site %s, publishing to %s)\n", *configPath, cfg.Site.ID, cfg.MQTT.Broker)
		return
	}
	pr := &probe.System{Cfg: probe.Config{
		Gateway: cfg.Probe.Gateway, Internet: cfg.Probe.Internet,
		DNSServer: cfg.Probe.DNSServer, DNSName: cfg.Probe.DNSName,
		TailscalePeer: cfg.Probe.TailscalePeer,
		PiHelperEnv:   cfg.Probe.PiHelperEnv,
		Count:         cfg.Probe.Count, Timeout: cfg.Probe.Timeout,
	}}
	if cfg.Probe.BridgeTopic != "" {
		pr.Bridge = &probe.BridgeWatch{Topic: cfg.Probe.BridgeTopic}
	}
	clock := outages.NewSystemClock()

	if *once {
		os.Exit(runOnce(cfg, pr, clock))
	}

	log.Printf("netmon %s starting (site %s, broker %s, probing every %s)",
		version, cfg.Site.ID, cfg.MQTT.Broker, cfg.Probe.Interval)
	tracker := outages.NewTracker(clock, store(cfg))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	pub, err := publisher.NewMQTTPublisher(publisher.Options{
		Broker: cfg.MQTT.Broker, ClientID: cfg.MQTT.ClientID,
		Username: cfg.MQTT.Username, Password: cfg.MQTT.Password,
		WillQOS: daemon.EventQOS,
	}, cfg.Topic("state"), daemon.WillPayload())
	if err != nil {
		log.Fatalf("configuring MQTT: %v", err)
	}
	if b := pr.Bridge; b != nil {
		pub.Watch(b.Topic, b.Set, b.Forget)
	}
	// The local broker may still be starting at boot; retry rather than exit
	// and lean on systemd's restart, whose rate limit gives up for good.
	if _, err := retry.Connect(ctx, "MQTT", startupBackoff, func() error { return pub.Connect(ctx) }, retry.Sleep); err != nil {
		log.Printf("MQTT connection interrupted: %v", err)
		return
	}
	log.Printf("connected to MQTT broker %s", cfg.MQTT.Broker)

	d := daemon.New(cfg, pr, pub, clock, tracker)
	run(ctx, d, pub, cfg.Probe.Interval)
}

func run(ctx context.Context, d *daemon.Daemon, pub *publisher.MQTTPublisher, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	d.Step(ctx)
	for {
		select {
		case <-ticker.C:
			d.Step(ctx)
		case <-pub.Reconnected():
			log.Printf("republishing after reconnecting")
			d.Reconnected()
			d.Step(ctx)
			ticker.Reset(interval)
		case <-ctx.Done():
			log.Println("shutting down")
			if !d.Offline() {
				log.Println("offline announcement failed; leaving the will to the broker")
			}
			pub.Close() //nolint:errcheck
			return
		}
	}
}

// runOnce probes once and prints the state payload: a dry run for checking
// the probes on a new host. It publishes nothing; with a bridge topic it
// reads the bridge's retained status from the broker first.
func runOnce(cfg config.Config, pr *probe.System, clock outages.Clock) int {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Probe.Interval)
	defer cancel()
	if pr.Bridge != nil {
		if err := readRetained(cfg, pr.Bridge.Topic, 3*time.Second, pr.Bridge.Set); err != nil {
			log.Printf("reading %s: %v", pr.Bridge.Topic, err)
		}
	}
	r := pr.Round(ctx)
	raw := classify.Raw(r, classify.Thresholds{LossPct: cfg.Probe.DegradedLossPct, RTTms: cfg.Probe.DegradedRTTms})
	var synced *bool
	if s, known := clock.Synced(); known {
		synced = &s
	}
	st := payload.NewState(clock.Now(), synced, string(raw), string(raw), nil, r)
	out, _ := json.MarshalIndent(st, "", "  ")
	fmt.Printf("%s\n%s\n", cfg.Topic("state"), out)
	return 0
}

func store(cfg config.Config) outages.Store {
	if cfg.StateDir == "" {
		log.Printf("no state directory: outages are not kept across restarts")
		return outages.NopStore{}
	}
	return &outages.FileStore{Path: filepath.Join(cfg.StateDir, outages.StateFileName)}
}

// readRetained connects once, without a will and under its own client id (so
// the running service is not disturbed), and passes the topic's retained
// message to set, waiting up to timeout for it.
func readRetained(cfg config.Config, topic string, timeout time.Duration, set func(string)) error {
	opts := mqtt.NewClientOptions().AddBroker(cfg.MQTT.Broker).
		SetClientID(cfg.MQTT.ClientID + "-once").SetConnectTimeout(timeout).SetAutoReconnect(false)
	if cfg.MQTT.Username != "" {
		opts.SetUsername(cfg.MQTT.Username).SetPassword(cfg.MQTT.Password)
	}
	c := mqtt.NewClient(opts)
	if tok := c.Connect(); !tok.WaitTimeout(timeout) || tok.Error() != nil {
		return fmt.Errorf("connecting to %s: %v", cfg.MQTT.Broker, tok.Error())
	}
	defer c.Disconnect(100)
	got := make(chan struct{}, 1)
	c.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) {
		set(string(m.Payload()))
		select {
		case got <- struct{}{}:
		default:
		}
	})
	select {
	case <-got:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("no retained message within %s", timeout)
	}
}
