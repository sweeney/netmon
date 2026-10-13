package publisher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// ── Fakes for paho's Client and Token interfaces ──────────────────────────
//
// These live in the internal test package so they can construct an
// MQTTPublisher directly; NewMQTTPublisher needs a real broker.

// fakeToken models a publish that either completes or never does.
type fakeToken struct {
	completes bool
	err       error
	done      chan struct{}
}

func newFakeToken(completes bool, err error) *fakeToken {
	ch := make(chan struct{})
	if completes {
		close(ch)
	}
	return &fakeToken{completes: completes, err: err, done: ch}
}

func (t *fakeToken) Wait() bool { <-t.done; return true }

// WaitTimeout reports false when the token does not complete in time, which is
// exactly the stall this timeout exists to bound.
func (t *fakeToken) WaitTimeout(d time.Duration) bool {
	select {
	case <-t.done:
		return true
	case <-time.After(d):
		return false
	}
}

// complete acknowledges a token that was left pending, as a late PUBACK or a
// dropped connection would.
func (t *fakeToken) complete() { close(t.done) }

func (t *fakeToken) Done() <-chan struct{} { return t.done }
func (t *fakeToken) Error() error          { return t.err }

// fakeClient implements mqtt.Client with only the behaviour the publisher needs.
type fakeClient struct {
	connected bool // the connection is open
	// reconnecting is paho auto-reconnecting: IsConnected() still says true
	// while IsConnectionOpen() says false.
	reconnecting bool
	token        mqtt.Token
	publishCalls int
	publishedQOS []byte
	connectErr   error
	connectHangs bool // models a blackholed route: the attempt never ends
	// connectErrs, when set, scripts successive Connect results; the last
	// one repeats.  A nil entry connects.
	connectErrs  []error
	connectCalls int
	disconnects  int
	// subscribed records each Subscribe, in order; subscribeErr fails them.
	subscribed   []fakeSub
	subscribeErr error
}

type fakeSub struct {
	topic   string
	qos     byte
	handler mqtt.MessageHandler
}

func (c *fakeClient) IsConnected() bool      { return c.connected || c.reconnecting }
func (c *fakeClient) IsConnectionOpen() bool { return c.connected }

func (c *fakeClient) Connect() mqtt.Token {
	c.connectCalls++
	if c.connectErrs != nil {
		err := c.connectErrs[0]
		if len(c.connectErrs) > 1 {
			c.connectErrs = c.connectErrs[1:]
		}
		if err == nil {
			c.connected = true
		}
		return newFakeToken(true, err)
	}
	return newFakeToken(!c.connectHangs, c.connectErr)
}
func (c *fakeClient) Disconnect(uint) {
	c.disconnects++
	c.connected = false
}

func (c *fakeClient) Publish(_ string, qos byte, _ bool, _ interface{}) mqtt.Token {
	c.publishCalls++
	c.publishedQOS = append(c.publishedQOS, qos)
	return c.token
}

func (c *fakeClient) Subscribe(topic string, qos byte, h mqtt.MessageHandler) mqtt.Token {
	c.subscribed = append(c.subscribed, fakeSub{topic, qos, h})
	return newFakeToken(true, c.subscribeErr)
}

func (c *fakeClient) SubscribeMultiple(map[string]byte, mqtt.MessageHandler) mqtt.Token {
	return newFakeToken(true, nil)
}

func (c *fakeClient) Unsubscribe(...string) mqtt.Token     { return newFakeToken(true, nil) }
func (c *fakeClient) AddRoute(string, mqtt.MessageHandler) {}
func (c *fakeClient) OptionsReader() mqtt.ClientOptionsReader {
	return mqtt.ClientOptionsReader{}
}

func testMsg() Message {
	return Message{Topic: "ups/test/ups/status", Payload: "OL", Retained: true}
}

// ── Tests ─────────────────────────────────────────────────────────────────

func TestPublish_Success(t *testing.T) {
	c := &fakeClient{connected: true, token: newFakeToken(true, nil)}
	p := &MQTTPublisher{client: c, timeout: time.Second}

	if err := p.Publish(testMsg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.publishCalls != 1 {
		t.Errorf("publishCalls = %d, want 1", c.publishCalls)
	}
}

// The regression this guards: token.Wait() had no deadline, so a publish that
// never completes blocked the poll loop for ever.
func TestPublish_TimesOutRatherThanBlocking(t *testing.T) {
	c := &fakeClient{connected: true, token: newFakeToken(false, nil)}
	p := &MQTTPublisher{client: c, timeout: 50 * time.Millisecond}

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- p.Publish(testMsg()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Errorf("error = %v, want a timeout", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("took %s to give up, want ~50ms", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked past its timeout — the poll loop would hang here")
	}
}

// A disconnected client must fail instantly.  With ~50 messages per poll,
// waiting out the timeout on each would stall the loop for minutes.
func TestPublish_DisconnectedFailsImmediately(t *testing.T) {
	c := &fakeClient{connected: false, token: newFakeToken(false, nil)}
	p := &MQTTPublisher{client: c, timeout: 30 * time.Second}

	start := time.Now()
	err := p.Publish(testMsg())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when disconnected")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("error = %v, want a not-connected error", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("took %s to fail, want immediate — the timeout must not be waited out", elapsed)
	}
	if c.publishCalls != 0 {
		t.Errorf("publishCalls = %d, want 0 — nothing should be sent while disconnected", c.publishCalls)
	}
}

// A broker-side error must still be surfaced.
func TestPublish_TokenErrorPropagated(t *testing.T) {
	want := errors.New("broker rejected the message")
	c := &fakeClient{connected: true, token: newFakeToken(true, want)}
	p := &MQTTPublisher{client: c, timeout: time.Second}

	if err := p.Publish(testMsg()); !errors.Is(err, want) {
		t.Errorf("error = %v, want %v", err, want)
	}
}

// A whole poll's worth of messages against a dead broker must fail fast rather
// than multiplying the timeout by the message count.
func TestPublish_FullPollAgainstDeadBrokerIsFast(t *testing.T) {
	c := &fakeClient{connected: false, token: newFakeToken(false, nil)}
	p := &MQTTPublisher{client: c, timeout: 5 * time.Second}

	start := time.Now()
	for i := 0; i < 52; i++ {
		if err := p.Publish(testMsg()); err == nil {
			t.Fatal("expected every publish to fail")
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("52 publishes took %s; a dead broker must not stall the poll loop", elapsed)
	}
}

func TestDefaultPublishTimeout_ShorterThanTypicalPoll(t *testing.T) {
	if DefaultPublishTimeout >= 10*time.Second {
		t.Errorf("DefaultPublishTimeout = %s, must be shorter than the 10s poll interval",
			DefaultPublishTimeout)
	}
}

// Each message goes at its own QoS, not one fixed for the client: telemetry
// and events share a connection but not a delivery guarantee.
func TestPublish_UsesPerMessageQOS(t *testing.T) {
	c := &fakeClient{connected: true, token: newFakeToken(true, nil)}
	p := &MQTTPublisher{client: c, timeout: time.Second}

	for _, q := range []byte{0, 1, 2} {
		msg := testMsg()
		msg.QOS = q
		if err := p.Publish(msg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := string(c.publishedQOS); got != string([]byte{0, 1, 2}) {
		t.Errorf("published at QoS %v, want [0 1 2]", c.publishedQOS)
	}
}

func TestConnect_ErrorNamesBroker(t *testing.T) {
	c := &fakeClient{connectErr: errors.New("no such host")}
	p := &MQTTPublisher{client: c, broker: "tcp://broker.invalid:1883"}

	err := p.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "tcp://broker.invalid:1883") {
		t.Errorf("Connect() = %v, want an error naming the broker", err)
	}
}

func TestConnect_Success(t *testing.T) {
	p := &MQTTPublisher{client: &fakeClient{}}
	if err := p.Connect(context.Background()); err != nil {
		t.Errorf("Connect() = %v, want nil", err)
	}
}

// A signal during a connect attempt must not wait out paho's 30 s
// ConnectTimeout: stopping the service against a blackholed broker would
// otherwise take that long.
func TestConnect_CancelledMidAttemptReturnsAtOnce(t *testing.T) {
	p := &MQTTPublisher{client: &fakeClient{connectHangs: true}, broker: "tcp://blackhole:1883"}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	start := time.Now()
	err := p.Connect(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Connect() = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s to return after cancel", elapsed)
	}
}
