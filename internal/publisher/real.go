// Adapted from github.com/sweeney/ups-mqtt internal/publisher (real.go):
// the reconnect and stall handling are unchanged.
package publisher

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/sweeney/netmon/internal/retry"
)

// DefaultPublishTimeout bounds how long a single publish may block.
//
// paho's token.Wait() has no deadline.  While a link is half-open — dead, but
// still looking open until the keep-alive notices — a QoS 1 publish never
// completes, so an unbounded wait stops the poll loop outright: a flaky link
// becomes total silence rather than a late message.  After one timeout the
// publisher stops waiting on further messages until the link recovers (see
// MQTTPublisher.stalled).
//
// It is deliberately shorter than any sensible poll interval: a poll that
// cannot finish before the next one begins is of no use to anyone.
const DefaultPublishTimeout = 5 * time.Second

// Tuning holds the connection timings.  Production uses DefaultTuning; tests
// shrink them so a dead link is noticed in seconds rather than a minute.
type Tuning struct {
	// KeepAlive is the MQTT keep-alive.  The broker declares a client dead —
	// and fires the LWT — after roughly 1.5x this with no traffic.  It is the
	// only signal that survives an outage which also takes out the uplink, so
	// it is kept short: ~45 s to notice rather than ~90 s, for one extra
	// PINGREQ a minute.  It also bounds how long this side takes to notice a
	// half-open connection: KeepAlive plus PingTimeout.
	KeepAlive   time.Duration
	PingTimeout time.Duration

	// PublishTimeout bounds how long a single publish may wait for its
	// acknowledgement (see DefaultPublishTimeout).
	PublishTimeout time.Duration

	// Reconnect is the backoff between attempts after a lost connection.
	// The cap is short: once the broker is back, the next poll should reach
	// it within a poll or two, not paho's default of up to ten minutes.
	Reconnect retry.Backoff
}

// DefaultTuning is the production Tuning.
func DefaultTuning() Tuning {
	return Tuning{
		KeepAlive:      30 * time.Second,
		PingTimeout:    10 * time.Second,
		PublishTimeout: DefaultPublishTimeout,
		Reconnect:      retry.Backoff{Initial: time.Second, Max: 30 * time.Second},
	}
}

// MQTTPublisher wraps paho.mqtt.golang and implements Publisher.  Each
// message carries its own QoS (see PublishConfig).
//
// It reconnects by itself rather than through paho's auto-reconnect.  While
// auto-reconnecting, paho reports IsConnected() as true and holds every
// QoS 1+ publish in its in-memory store, then resends the lot on reconnect
// in map order.  A live test (docs/live-tests.md) saw 6½ hours of backlog —
// ~4,650 messages — land on the broker in two seconds, shuffled, with a
// stale outage payload able to arrive after its clear.  Each of those
// publishes also waited out the timeout, stretching a 10 s poll to 30 s.
//
// With auto-reconnect off, a lost connection leaves the client disconnected,
// so publishes fail at once; and an explicit Connect on a clean session
// empties paho's store, so nothing stale is ever resent.  What was missed is
// made good by the caller republishing current state on Reconnected.
type MQTTPublisher struct {
	client    mqtt.Client
	broker    string
	timeout   time.Duration
	reconnect retry.Backoff
	wait      retry.Wait

	reconnected chan struct{}

	mu sync.Mutex
	// ctx bounds the reconnect loop; it is the context of the first Connect,
	// which is the process's signal context.
	ctx context.Context
	// stalled is a publish that timed out without an acknowledgement.  Until
	// it completes, or the connection is replaced, further publishes fail at
	// once: a link that swallowed one message will swallow the rest, and
	// waiting out the timeout on each is what stretched the polls.
	stalled   mqtt.Token
	stalledAt time.Time
	// wedgeAfter is how long a stall may last before the connection is
	// replaced.  Past KeepAlive plus PingTimeout the link has proved itself
	// alive (or paho would have dropped it) yet still not acknowledged the
	// publish, so waiting longer will not help.
	wedgeAfter time.Duration

	// lastFailed is whether the most recent publish failed.  Close then
	// leaves without a clean DISCONNECT, so the broker publishes the will:
	// the offline announcement it stands in for did not get through.
	lastFailed bool
	// closed stops a reconnect in flight from outliving Close.
	closed bool

	watches []watch
}

// watch is a subscription made on every (re)connect.
type watch struct {
	topic  string
	onMsg  func(payload string)
	onLost func()
}

// NewMQTTPublisher creates an MQTT client with DefaultTuning but does not
// connect it; call Connect.  It fails only on configuration errors that no
// retry can fix, such as an unreadable CA certificate.
// lwtTopic and lwtPayload are used for the Last Will and Testament message,
// published by the broker if the client disconnects unexpectedly.  The LWT is
// an event, so it goes at the event QoS.
func NewMQTTPublisher(cfg Options, lwtTopic, lwtPayload string) (*MQTTPublisher, error) {
	return NewMQTTPublisherWith(cfg, lwtTopic, lwtPayload, DefaultTuning())
}

// NewMQTTPublisherWith is NewMQTTPublisher with explicit timings.
func NewMQTTPublisherWith(cfg Options, lwtTopic, lwtPayload string, tn Tuning) (*MQTTPublisher, error) {
	p := &MQTTPublisher{
		broker:      cfg.Broker,
		timeout:     tn.PublishTimeout,
		reconnect:   tn.Reconnect,
		wait:        retry.Sleep,
		reconnected: make(chan struct{}, 1),
		wedgeAfter:  tn.KeepAlive + tn.PingTimeout,
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.Broker)
	opts.SetClientID(cfg.ClientID)
	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}
	opts.SetKeepAlive(tn.KeepAlive)
	opts.SetPingTimeout(tn.PingTimeout)
	// Without a write timeout paho's Publish can block for up to 30 s
	// handing a message to a writer stuck on a full socket buffer.
	opts.SetWriteTimeout(tn.PublishTimeout)
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(false)
	opts.SetConnectionLostHandler(p.connectionLost)
	opts.SetOnConnectHandler(p.subscribe)
	opts.SetWill(lwtTopic, lwtPayload, cfg.WillQOS, true)

	if cfg.TLSCACert != "" {
		tlsCfg, err := newTLSConfig(cfg.TLSCACert)
		if err != nil {
			return nil, fmt.Errorf("loading TLS CA cert %q: %w", cfg.TLSCACert, err)
		}
		opts.SetTLSConfig(tlsCfg)
	}

	p.client = mqtt.NewClient(opts)
	return p, nil
}

// Connect makes one attempt to connect to the broker.  The caller retries:
// at boot the broker may not be up yet, or its name may not resolve.  Once
// connected, a lost connection is retried by the publisher itself, bounded
// by the ctx of this first call; Reconnected signals each success.
//
// An attempt can take up to paho's ConnectTimeout (30 s) against a
// blackholed route, so it returns as soon as ctx is cancelled rather than
// holding up shutdown for that long.  The abandoned attempt is left to paho;
// the process is exiting.
//
// A CONNACK refusal (bad credentials, not authorised) is returned like any
// other failure and so is retried.  That is deliberate: exiting would only
// hand the retry to systemd, whose start-rate limit gives up for good.
func (p *MQTTPublisher) Connect(ctx context.Context) error {
	p.mu.Lock()
	if p.ctx == nil {
		p.ctx = ctx
	}
	p.mu.Unlock()
	return p.connectOnce(ctx)
}

func (p *MQTTPublisher) connectOnce(ctx context.Context) error {
	token := p.client.Connect()
	select {
	case <-token.Done():
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("connecting to MQTT broker %q: %w", p.broker, err)
	}
	return nil
}

// Watch subscribes to topic on this connection, now if it is open and again
// on every reconnect, so a retained message is delivered each time. onLost
// runs when the connection drops, since changes made while it is down are
// missed. Call it before Connect.
func (p *MQTTPublisher) Watch(topic string, onMsg func(payload string), onLost func()) {
	p.mu.Lock()
	p.watches = append(p.watches, watch{topic, onMsg, onLost})
	p.mu.Unlock()
}

// lostWatches tells every watch its connection has gone.
func (p *MQTTPublisher) lostWatches() {
	p.mu.Lock()
	ws := append([]watch(nil), p.watches...)
	p.mu.Unlock()
	for _, w := range ws {
		if w.onLost != nil {
			w.onLost()
		}
	}
}

// subscribe is paho's OnConnect handler: (re)subscribe every watch.
func (p *MQTTPublisher) subscribe(c mqtt.Client) {
	p.mu.Lock()
	ws := append([]watch(nil), p.watches...)
	p.mu.Unlock()
	for _, w := range ws {
		w := w
		tok := c.Subscribe(w.topic, 1, func(_ mqtt.Client, m mqtt.Message) { w.onMsg(string(m.Payload())) })
		go func() {
			if !tok.WaitTimeout(p.timeout) || tok.Error() != nil {
				log.Printf("subscribing to %s on %s failed: %v", w.topic, p.broker, tok.Error())
			}
		}()
	}
}

// Reconnected receives a value each time a lost connection is re-established.
// The caller should republish current state then: anything published while
// the connection was down failed and was not queued.
func (p *MQTTPublisher) Reconnected() <-chan struct{} { return p.reconnected }

// connectionLost is paho's handler for an unexpected disconnect (not one
// asked for by Close).  It runs on its own goroutine.
func (p *MQTTPublisher) connectionLost(_ mqtt.Client, err error) {
	log.Printf("MQTT connection to %s lost: %v — reconnecting", p.broker, err)
	p.lostWatches()
	p.reconnectLoop(time.Now())
}

// reconnectLoop retries until connected, the publisher's context ends, or
// it is closed.
func (p *MQTTPublisher) reconnectLoop(lostAt time.Time) {
	p.mu.Lock()
	ctx, closed := p.ctx, p.closed
	p.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if closed || ctx.Err() != nil {
		return // shutting down
	}

	res, err := retry.Connect(ctx, "MQTT", p.reconnect, func() error { return p.connectOnce(ctx) }, p.wait)
	if err != nil {
		return // shutting down
	}

	p.mu.Lock()
	closed = p.closed
	p.stalled = nil // its connection is gone; the new one starts clean
	p.mu.Unlock()
	if closed {
		p.client.Disconnect(0) // connected just as Close ran
		return
	}
	log.Printf("reconnected to MQTT broker %s after %s (%d attempts)",
		p.broker, time.Since(lostAt).Round(time.Second), res.Attempts)

	select {
	case p.reconnected <- struct{}{}:
	default: // one pending signal is enough
	}
}

// Publish sends a single MQTT message and waits for the broker to acknowledge,
// up to the publisher's timeout.
//
// It fails at once, without sending, when the connection is not open (paho's
// IsConnectionOpen, not IsConnected — see MQTTPublisher), or while an earlier
// publish is still unacknowledged past its timeout.  With ~10 messages per
// poll, waiting out the timeout on each would stall the poll loop.  Nothing is
// lost by giving up: the caller republishes current state on Reconnected,
// which is what a subscriber wants.
func (p *MQTTPublisher) Publish(msg Message) error {
	err := p.publish(msg)
	p.mu.Lock()
	p.lastFailed = err != nil
	p.mu.Unlock()
	return err
}

func (p *MQTTPublisher) publish(msg Message) error {
	if !p.client.IsConnectionOpen() {
		return fmt.Errorf("publishing to %q: not connected to broker", msg.Topic)
	}
	if p.isStalled() {
		return fmt.Errorf("publishing to %q: skipped — an earlier publish is still unacknowledged after %s", msg.Topic, p.timeout)
	}
	token := p.client.Publish(msg.Topic, msg.QOS, msg.Retained, msg.Payload)
	if !token.WaitTimeout(p.timeout) {
		p.mu.Lock()
		p.stalled, p.stalledAt = token, time.Now()
		p.mu.Unlock()
		return fmt.Errorf("publishing to %q: timed out after %s", msg.Topic, p.timeout)
	}
	return token.Error()
}

// isStalled reports whether a timed-out publish is still unacknowledged.  A
// stall that outlasts wedgeAfter replaces the connection.
func (p *MQTTPublisher) isStalled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stalled == nil {
		return false
	}
	select {
	case <-p.stalled.Done():
		p.stalled = nil // acknowledged late, or failed with the connection
		return false
	default:
	}
	if p.wedgeAfter > 0 && time.Since(p.stalledAt) > p.wedgeAfter && !p.closed {
		log.Printf("MQTT broker %s has not acknowledged a publish for %s although the connection looks open — reconnecting",
			p.broker, time.Since(p.stalledAt).Round(time.Second))
		p.stalled = nil
		go func() {
			p.client.Disconnect(0)
			p.lostWatches()
			p.reconnectLoop(time.Now())
		}()
	}
	return true
}

// Close disconnects from the broker.  The disconnect is a clean one only if
// the last publish — normally the offline announcement — got through.
// Otherwise the connection is left for the process exit to drop, without a
// DISCONNECT, so the broker publishes the will in its place.
func (p *MQTTPublisher) Close() error {
	p.mu.Lock()
	p.closed = true
	failed := p.lastFailed
	p.mu.Unlock()
	if failed && p.client.IsConnectionOpen() {
		log.Printf("leaving MQTT without a clean disconnect so the broker publishes the will")
		return nil
	}
	p.client.Disconnect(250)
	return nil
}

// newTLSConfig builds a *tls.Config that trusts caFile as an additional CA.
func newTLSConfig(caFile string) (*tls.Config, error) {
	caCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA cert from %q", caFile)
	}
	return &tls.Config{RootCAs: pool}, nil
}
