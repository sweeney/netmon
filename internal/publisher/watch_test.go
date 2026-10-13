package publisher

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a log destination safe to write from paho's goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeMessage is an incoming message, as paho hands one to a handler.
type fakeMessage struct{ topic, payload string }

func (m fakeMessage) Duplicate() bool   { return false }
func (m fakeMessage) Qos() byte         { return 1 }
func (m fakeMessage) Retained() bool    { return true }
func (m fakeMessage) Topic() string     { return m.topic }
func (m fakeMessage) MessageID() uint16 { return 1 }
func (m fakeMessage) Payload() []byte   { return []byte(m.payload) }
func (m fakeMessage) Ack()              {}

// Every watch is subscribed on every connect, at QoS 1, and its messages
// reach onMsg: a clean session forgets subscriptions, and the retained
// status only arrives with a fresh subscribe.
func TestWatch_SubscribesOnEveryConnect(t *testing.T) {
	c := &fakeClient{connected: true}
	var waits []time.Duration
	p := newTestPublisher(c, &waits)
	var got []string
	p.Watch("sites/a/bridge", func(s string) { got = append(got, "a="+s) }, nil)
	p.Watch("sites/b/bridge", func(s string) { got = append(got, "b="+s) }, nil)

	p.subscribe(c) // paho's OnConnect
	if len(c.subscribed) != 2 || c.subscribed[0].topic != "sites/a/bridge" || c.subscribed[1].topic != "sites/b/bridge" {
		t.Fatalf("subscribed %+v, want both watches", c.subscribed)
	}
	for _, s := range c.subscribed {
		if s.qos != 1 {
			t.Errorf("%s subscribed at QoS %d, want 1", s.topic, s.qos)
		}
	}
	c.subscribed[0].handler(c, fakeMessage{"sites/a/bridge", "1"})
	c.subscribed[1].handler(c, fakeMessage{"sites/b/bridge", "0"})
	if strings.Join(got, ",") != "a=1,b=0" {
		t.Errorf("onMsg got %v, want a=1,b=0", got)
	}

	p.subscribe(c) // a reconnect
	if len(c.subscribed) != 4 {
		t.Errorf("after a reconnect: %d subscribes, want 4", len(c.subscribed))
	}
}

// A failed subscribe is logged; the watch stays unknown until the next
// connect retries it.
func TestWatch_SubscribeFailureLogged(t *testing.T) {
	buf := &lockedBuffer{}
	out := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(out) })
	c := &fakeClient{connected: true, subscribeErr: errors.New("not authorised")}
	var waits []time.Duration
	p := newTestPublisher(c, &waits)
	p.Watch("sites/a/bridge", func(string) {}, nil)
	p.subscribe(c)
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), "subscribing to sites/a/bridge on tcp://broker:1883 failed: not authorised") {
		if time.Now().After(deadline) {
			t.Fatalf("failure not logged:\n%s", buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A lost connection marks every watch unknown before the reconnect starts:
// a change while it is down would be missed.
func TestWatch_ConnectionLostForgets(t *testing.T) {
	captureLog(t)
	c := &fakeClient{connected: true, connectErrs: []error{nil}}
	var waits []time.Duration
	p := newTestPublisher(c, &waits)
	var events []string
	p.Watch("sites/a/bridge", func(string) {}, func() { events = append(events, "lost a") })
	p.Watch("sites/b/bridge", func(string) {}, nil) // no onLost: skipped
	p.connectionLost(c, errors.New("EOF"))
	select {
	case <-p.Reconnected():
	default:
		t.Fatal("no reconnect")
	}
	if strings.Join(events, ",") != "lost a" {
		t.Errorf("events %v, want one lost", events)
	}
}

// The wedge guard replaces the connection itself, with no connectionLost
// from paho, so it must forget the watches too.
func TestWatch_WedgeForgets(t *testing.T) {
	captureLog(t)
	c := &fakeClient{connected: true, token: newFakeToken(false, nil), connectErrs: []error{nil}}
	var waits []time.Duration
	p := newTestPublisher(c, &waits)
	p.timeout, p.wedgeAfter = 10*time.Millisecond, 30*time.Millisecond
	lost := make(chan struct{}, 1)
	p.Watch("sites/a/bridge", func(string) {}, func() { lost <- struct{}{} })

	p.Publish(testMsg()) //nolint:errcheck // times out: stalled
	time.Sleep(50 * time.Millisecond)
	p.Publish(testMsg()) //nolint:errcheck // notices the wedge
	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("wedge did not forget the watch")
	}
	<-p.Reconnected()
}
