package publisher

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/netmon/internal/retry"
)

// captureLog redirects the standard logger for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	return &buf
}

// newTestPublisher is an MQTTPublisher over c whose reconnect backoff is
// recorded rather than slept.
func newTestPublisher(c *fakeClient, waits *[]time.Duration) *MQTTPublisher {
	return &MQTTPublisher{
		client:      c,
		broker:      "tcp://broker:1883",
		timeout:     50 * time.Millisecond,
		reconnect:   DefaultTuning().Reconnect,
		wait:        func(ctx context.Context, d time.Duration) error { *waits = append(*waits, d); return ctx.Err() },
		reconnected: make(chan struct{}, 1),
		ctx:         context.Background(),
	}
}

// paho's IsConnected() is true while it auto-reconnects.  Publishing on that
// signal queued every message for hours and replayed them shuffled
// (docs/live-tests.md, test 2); the connection must actually be open.
func TestPublish_ReconnectingFailsImmediately(t *testing.T) {
	c := &fakeClient{reconnecting: true, token: newFakeToken(false, nil)}
	p := &MQTTPublisher{client: c, timeout: 30 * time.Second}

	start := time.Now()
	err := p.Publish(testMsg())
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("Publish() = %v, want a not-connected error", err)
	}
	if c.publishCalls != 0 {
		t.Errorf("publishCalls = %d, want 0 — nothing may be queued while reconnecting", c.publishCalls)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("took %s, want immediate", elapsed)
	}
}

// A half-open link looks open until the keep-alive notices.  One timeout says
// the link is swallowing messages; the rest of the poll must not each wait
// out the timeout too — that is what stretched 10 s polls to 30 s.
func TestPublish_StalledLinkFailsRestOfPollFast(t *testing.T) {
	c := &fakeClient{connected: true, token: newFakeToken(false, nil)}
	p := &MQTTPublisher{client: c, timeout: 50 * time.Millisecond}

	start := time.Now()
	for i := 0; i < 10; i++ {
		err := p.Publish(testMsg())
		if err == nil {
			t.Fatalf("publish %d succeeded on a stalled link", i)
		}
		if i == 0 && !strings.Contains(err.Error(), "timed out") {
			t.Errorf("first error = %v, want a timeout", err)
		}
		if i > 0 && !strings.Contains(err.Error(), "unacknowledged") {
			t.Errorf("error %d = %v, want a fast skip naming the unacknowledged publish", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("10 publishes took %s; only the first should wait out the 50ms timeout", elapsed)
	}
	if c.publishCalls != 1 {
		t.Errorf("publishCalls = %d, want 1 — nothing more is handed to a stalled link", c.publishCalls)
	}
}

// A late acknowledgement means the link was only slow: publishing resumes.
func TestPublish_StallClearsOnLateAck(t *testing.T) {
	pending := newFakeToken(false, nil)
	c := &fakeClient{connected: true, token: pending}
	p := &MQTTPublisher{client: c, timeout: 20 * time.Millisecond}

	if err := p.Publish(testMsg()); err == nil {
		t.Fatal("expected a timeout")
	}
	pending.complete()
	c.token = newFakeToken(true, nil)

	if err := p.Publish(testMsg()); err != nil {
		t.Errorf("Publish() after the late ack = %v, want nil", err)
	}
	if c.publishCalls != 2 {
		t.Errorf("publishCalls = %d, want 2", c.publishCalls)
	}
}

// A lost connection is retried by the publisher with the reconnect backoff,
// logged, cleared of any stall, and signalled so the caller can republish.
func TestConnectionLost_ReconnectsAndSignals(t *testing.T) {
	buf := captureLog(t)
	noHost := errors.New("dial tcp: lookup broker: no such host")
	c := &fakeClient{connectErrs: []error{noHost, noHost, nil}}
	var waits []time.Duration
	p := newTestPublisher(c, &waits)
	p.stalled = newFakeToken(false, nil)

	p.connectionLost(c, errors.New("pingresp not received, disconnecting"))

	select {
	case <-p.Reconnected():
	default:
		t.Fatal("no Reconnected signal after reconnecting")
	}
	if c.connectCalls != 3 {
		t.Errorf("connect called %d times, want 3", c.connectCalls)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Errorf("waits = %v, want [1s 2s]", waits)
	}
	if p.isStalled() {
		t.Error("still stalled after reconnecting; the new connection starts clean")
	}
	out := buf.String()
	for _, want := range []string{
		"MQTT connection to tcp://broker:1883 lost: pingresp not received",
		"no such host",
		"reconnected to MQTT broker tcp://broker:1883",
		"(3 attempts)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// Shutdown during reconnection ends the loop without a signal.
func TestConnectionLost_StopsOnShutdown(t *testing.T) {
	captureLog(t)
	c := &fakeClient{connectErr: errors.New("connection refused")}
	ctx, cancel := context.WithCancel(context.Background())
	p := &MQTTPublisher{
		client:      c,
		broker:      "tcp://broker:1883",
		reconnect:   DefaultTuning().Reconnect,
		wait:        retry.Sleep,
		reconnected: make(chan struct{}, 1),
		ctx:         ctx,
	}
	time.AfterFunc(20*time.Millisecond, cancel)

	done := make(chan struct{})
	go func() { p.connectionLost(c, errors.New("EOF")); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect loop did not stop on shutdown")
	}
	select {
	case <-p.Reconnected():
		t.Error("Reconnected signalled although no connection was made")
	default:
	}
}

// Reconnected holds one pending signal, never blocking the reconnect loop.
func TestReconnected_DoesNotBlock(t *testing.T) {
	captureLog(t)
	c := &fakeClient{connectErrs: []error{nil}}
	var waits []time.Duration
	p := newTestPublisher(c, &waits)
	p.connectionLost(c, errors.New("EOF"))
	p.connectionLost(c, errors.New("EOF")) // nobody has read the first signal
	<-p.Reconnected()
	select {
	case <-p.Reconnected():
		t.Error("two signals pending, want one")
	default:
	}
}

// paho must not auto-reconnect: doing so queues and replays publishes.  The
// session is clean, so an explicit reconnect empties paho's store.
func TestNewMQTTPublisher_ManualReconnectCleanSession(t *testing.T) {
	cfg := Options{Broker: "tcp://127.0.0.1:1", ClientID: "test"}
	p, err := NewMQTTPublisher(cfg, "ups/x/state", "{}")
	if err != nil {
		t.Fatal(err)
	}
	opts := p.client.OptionsReader()
	if opts.AutoReconnect() {
		t.Error("paho auto-reconnect is on; it queues publishes and replays them out of order")
	}
	if !opts.CleanSession() {
		t.Error("session is not clean; paho would resend stored publishes on reconnect")
	}
	if opts.KeepAlive() != 30*time.Second || opts.PingTimeout() != 10*time.Second {
		t.Errorf("keep-alive %s, ping timeout %s; want 30s and 10s", opts.KeepAlive(), opts.PingTimeout())
	}
	if p.reconnect.Max != 30*time.Second {
		t.Errorf("reconnect cap = %s, want 30s", p.reconnect.Max)
	}
}

// A shutdown whose offline announcement failed must not disconnect cleanly:
// a clean DISCONNECT makes the broker discard the will, and the retained
// state would go on saying online.
func TestClose_SkipsCleanDisconnectAfterFailedPublish(t *testing.T) {
	captureLog(t)
	c := &fakeClient{connected: true, token: newFakeToken(false, nil)}
	p := &MQTTPublisher{client: c, timeout: 10 * time.Millisecond}
	if err := p.Publish(testMsg()); err == nil {
		t.Fatal("expected the publish to time out")
	}
	p.Close() //nolint:errcheck
	if c.disconnects != 0 {
		t.Error("disconnected cleanly after a failed offline announcement; the broker would drop the will")
	}
}

func TestClose_DisconnectsCleanlyAfterSuccessfulPublish(t *testing.T) {
	c := &fakeClient{connected: true, token: newFakeToken(true, nil)}
	p := &MQTTPublisher{client: c, timeout: time.Second}
	if err := p.Publish(testMsg()); err != nil {
		t.Fatal(err)
	}
	p.Close() //nolint:errcheck
	if c.disconnects != 1 {
		t.Errorf("disconnects = %d, want a clean disconnect", c.disconnects)
	}
}

// A stall that outlasts the keep-alive means the broker answers pings but
// not publishes.  Waiting will not help; the connection is replaced.
func TestPublish_WedgedStallReplacesConnection(t *testing.T) {
	buf := captureLog(t)
	c := &fakeClient{connected: true, token: newFakeToken(false, nil), connectErrs: []error{nil}}
	var waits []time.Duration
	p := newTestPublisher(c, &waits)
	p.timeout = 10 * time.Millisecond
	p.wedgeAfter = 30 * time.Millisecond

	if err := p.Publish(testMsg()); err == nil {
		t.Fatal("expected a timeout")
	}
	if err := p.Publish(testMsg()); err == nil || !strings.Contains(err.Error(), "unacknowledged") {
		t.Fatalf("Publish() = %v, want a fast skip while stalled", err)
	}
	time.Sleep(50 * time.Millisecond)
	p.Publish(testMsg()) //nolint:errcheck // notices the wedge

	select {
	case <-p.Reconnected():
	case <-time.After(2 * time.Second):
		t.Fatal("wedged connection not replaced")
	}
	if c.disconnects != 1 || c.connectCalls != 1 {
		t.Errorf("disconnects %d, connects %d; want the connection dropped and remade once", c.disconnects, c.connectCalls)
	}
	if !strings.Contains(buf.String(), "has not acknowledged a publish") {
		t.Errorf("wedge not logged:\n%s", buf.String())
	}
}

// A connection lost after Close does not start a reconnect.
func TestConnectionLost_AfterCloseDoesNotReconnect(t *testing.T) {
	captureLog(t)
	c := &fakeClient{connected: true, connectErrs: []error{nil}}
	var waits []time.Duration
	p := newTestPublisher(c, &waits)
	p.Close() //nolint:errcheck
	p.connectionLost(c, errors.New("EOF"))
	if c.connectCalls != 0 {
		t.Errorf("connect called %d times after Close", c.connectCalls)
	}
}
