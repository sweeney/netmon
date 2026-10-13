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

// Hysteresis holds a status until enough consecutive rounds point away from
// it: DownAfter rounds worse than it to get worse, UpAfter rounds better than
// it to get better. The rounds in a run need not agree with each other (a
// flaky gateway during a WAN outage alternates lan_down and wan_down); the run
// settles on the worst status it saw, dated from its first round. Before the
// first status settles, a run is rounds on the same side of "outage": DownAfter
// of them for an outage, UpAfter otherwise.
type Hysteresis struct {
	DownAfter int
	UpAfter   int

	current Status
	dir     int    // the run's direction: +1 worse, -1 better, 0 none
	worst   Status // the worst status in the run
	count   int
	since   time.Duration
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
// began: the reading (any monotonic clock) passed with the run's first round.
func (h *Hysteresis) ObserveAt(raw Status, at time.Duration) (Status, bool, time.Duration) {
	var dir, need int
	switch {
	case h.current == Unknown && raw.IsOutage():
		dir, need = 1, h.DownAfter
	case h.current == Unknown:
		dir, need = -1, h.UpAfter
	case raw.Worse(h.current):
		dir, need = 1, h.DownAfter
	case h.current.Worse(raw):
		dir, need = -1, h.UpAfter
	default: // the current status again: any run is over
		h.dir, h.count = 0, 0
		return h.current, false, 0
	}
	if dir != h.dir || h.count == 0 {
		h.dir, h.count, h.worst, h.since = dir, 0, raw, at
	}
	h.count++
	if raw.Worse(h.worst) {
		h.worst = raw
	}
	if need < 1 {
		need = 1
	}
	if h.count < need {
		return h.current, false, 0
	}
	h.current, h.dir, h.count = h.worst, 0, 0
	return h.current, true, h.since
}
