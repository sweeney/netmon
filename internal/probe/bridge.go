package probe

import (
	"strings"
	"sync"
)

// BridgeWatch holds the latest status of the site's MQTT bridge, as Mosquitto
// publishes it, retained, on its notification topic: "1" connected, "0" not.
// netmon reads it over the connection it already has to its local broker,
// so the check costs nothing on the wire and leaves nothing in either
// broker's log, unlike a probe that connects every round.
//
// The bridge notices a dead link only when its keepalive lapses (about 45 s
// with keepalive_interval 30), and reconnects with backoff, so this check
// lags the link by up to a minute either way.
type BridgeWatch struct {
	Topic string

	mu      sync.Mutex
	payload string
	known   bool
}

// Set records a message from the notification topic.
func (b *BridgeWatch) Set(payload string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.payload, b.known = strings.TrimSpace(payload), true
}

// Forget marks the status unknown: the connection it came over has gone, so
// later changes would be missed.
func (b *BridgeWatch) Forget() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.payload, b.known = "", false
}

// Result is the bridge check for this round. An unknown status fails it:
// without the local broker the site cannot publish anything either.
func (b *BridgeWatch) Result() Result {
	b.mu.Lock()
	defer b.mu.Unlock()
	res := Result{Target: b.Topic}
	switch {
	case !b.known:
		res.Err = "bridge status unknown (no message from the local broker)"
	case b.payload == "1":
		res.OK = true
	case b.payload == "0":
		res.Err = "bridge reports its link down"
	default:
		res.Err = "unexpected bridge status " + quote(b.payload)
	}
	return res
}

func quote(s string) string { return `"` + s + `"` }
