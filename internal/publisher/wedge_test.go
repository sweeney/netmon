package publisher

import "testing"

// The guard waits twice paho's own dead-link detection, so paho handles an
// ordinary dead link and the guard only a wedged one.
func TestWedgeAfterIsTwiceThePingDeadline(t *testing.T) {
	tn := DefaultTuning()
	p, err := NewMQTTPublisherWith(Options{Broker: "tcp://127.0.0.1:1", ClientID: "test"}, "t", "{}", tn)
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * (tn.KeepAlive + tn.PingTimeout); p.wedgeAfter != want {
		t.Errorf("wedgeAfter %s, want %s", p.wedgeAfter, want)
	}
}
