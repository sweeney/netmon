// Package classify turns one round of probe results into a single network
// status, and smooths that status over rounds so that one dropped packet does
// not become an outage.
package classify

import (
	"time"

	"github.com/sweeney/netmon/internal/probe"
)

// Status is the network's state as seen from this host.
type Status string

const (
	Unknown     Status = ""             // not enough rounds yet to say
	OK          Status = "ok"           // every check passed
	Degraded    Status = "degraded"     // every check passed, but loss or latency is high
	TailnetDown Status = "tailnet_down" // internet up, broker unreachable
	WANDown     Status = "wan_down"     // gateway up, internet unreachable
	LANDown     Status = "lan_down"     // gateway unreachable
)

// severity orders statuses from best to worst.
func (s Status) severity() int {
	switch s {
	case OK:
		return 1
	case Degraded:
		return 2
	case TailnetDown:
		return 3
	case WANDown:
		return 4
	case LANDown:
		return 5
	}
	return 0
}

// IsOutage reports whether s is an outage: something on the path to the
// broker is down. Degraded is shown live but is not an outage.
func (s Status) IsOutage() bool { return s.severity() >= TailnetDown.severity() }

// Worse reports whether s is worse than t.
func (s Status) Worse(t Status) bool { return s.severity() > t.severity() }

// Thresholds mark a working network as degraded.
type Thresholds struct {
	LossPct float64 // any check losing more than this
	RTTms   float64 // any check slower than this
}

// Raw classifies one round, checking from this host outwards: a failure
// close by explains every failure further out.
func Raw(r probe.Round, t Thresholds) Status {
	switch {
	case !r.Gateway.OK:
		return LANDown
	case !r.Internet.OK:
		return WANDown
	case !r.Broker.OK:
		return TailnetDown
	}
	for _, c := range []probe.Result{r.Gateway, r.Internet, r.DNS, r.Broker} {
		if c.LossPct != nil && t.LossPct > 0 && *c.LossPct > t.LossPct {
			return Degraded
		}
		if c.RTTms != nil && t.RTTms > 0 && *c.RTTms > t.RTTms {
			return Degraded
		}
	}
	// A failing DNS check, with everything else up, is degraded: names
	// outside the tailnet do not resolve, but the path to the broker works.
	if !r.DNS.OK {
		return Degraded
	}
	return OK
}

// Hysteresis holds a status until another one has been seen for enough
// consecutive rounds: DownAfter rounds to get worse, UpAfter to get better.
// The first status needs UpAfter rounds if it is ok or degraded, DownAfter if
// it is an outage.
type Hysteresis struct {
	DownAfter int
	UpAfter   int

	current   Status
	candidate Status
	count     int
	since     time.Duration
}

// Current is the smoothed status; Unknown until the first one settles.
func (h *Hysteresis) Current() Status { return h.current }

// Observe feeds one round's raw status and returns the smoothed status and
// whether it changed with this round.
func (h *Hysteresis) Observe(raw Status) (Status, bool) {
	st, changed, _ := h.ObserveAt(raw, 0)
	return st, changed
}

// ObserveAt is Observe that also returns, on a change, when the new status
// began: the reading of the first round in the run that settled it. A
// status is dated from its first round, not from when it was confirmed.
func (h *Hysteresis) ObserveAt(raw Status, at time.Duration) (Status, bool, time.Duration) {
	if raw == h.current {
		h.candidate, h.count = Unknown, 0
		return h.current, false, 0
	}
	if raw != h.candidate {
		h.candidate, h.count, h.since = raw, 0, at
	}
	h.count++
	need := h.UpAfter
	if raw.Worse(h.current) && h.current != Unknown || h.current == Unknown && raw.IsOutage() {
		need = h.DownAfter
	}
	if need < 1 {
		need = 1
	}
	if h.count < need {
		return h.current, false, 0
	}
	h.current, h.candidate, h.count = raw, Unknown, 0
	return h.current, true, h.since
}
