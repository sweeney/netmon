// Package payload builds the JSON published on the site's network topics.
// Unknown values are absent, not zero, as in ups-mqtt.
package payload

import (
	"encoding/json"
	"time"

	"github.com/sweeney/netmon/internal/outages"
	"github.com/sweeney/netmon/internal/probe"
)

// Check is one check in the state payload.
type Check struct {
	OK        bool     `json:"ok"`
	Target    string   `json:"target,omitempty"`
	Targets   *int     `json:"targets,omitempty"`
	TargetsOK *int     `json:"targets_ok,omitempty"`
	RTTms     *float64 `json:"rtt_ms,omitempty"`
	LossPct   *float64 `json:"loss_pct,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Tailscale is Tailscale's path to the broker's host.
type Tailscale struct {
	Path          string     `json:"path,omitempty"`
	Endpoint      string     `json:"endpoint,omitempty"`
	Relay         string     `json:"relay,omitempty"`
	Online        *bool      `json:"online,omitempty"`
	LastHandshake *time.Time `json:"last_handshake,omitempty"`
}

// State is published every round.
type State struct {
	Timestamp   time.Time        `json:"timestamp"`
	ClockSynced *bool            `json:"clock_synced,omitempty"`
	Online      bool             `json:"online"`
	Status      string           `json:"status"`
	StatusSince *time.Time       `json:"status_since,omitempty"`
	RawStatus   string           `json:"raw_status"`
	Checks      map[string]Check `json:"checks"`
	Tailscale   *Tailscale       `json:"tailscale,omitempty"`
}

// NewState builds the state payload. status is the smoothed status ("" while
// not yet settled, published as "unknown"); raw is this round's.
func NewState(now time.Time, synced *bool, status, raw string, since *time.Time, r probe.Round) State {
	if status == "" {
		status = "unknown"
	}
	s := State{
		Timestamp:   now.UTC(),
		ClockSynced: synced,
		Online:      true,
		Status:      status,
		RawStatus:   raw,
		Checks: map[string]Check{
			"gateway":  check(r.Gateway, false),
			"internet": check(r.Internet, true),
			"dns":      check(r.DNS, false),
			"broker":   check(r.Broker, false),
		},
	}
	if since != nil {
		u := since.UTC()
		s.StatusSince = &u
	}
	if ts := r.Tailscale; ts != nil {
		s.Tailscale = &Tailscale{Path: ts.Path, Endpoint: ts.Endpoint, Relay: ts.Relay, Online: ts.Online, LastHandshake: ts.LastHandshake}
	}
	return s
}

func check(r probe.Result, multi bool) Check {
	c := Check{OK: r.OK, Target: r.Target, RTTms: r.RTTms, LossPct: r.LossPct, Error: r.Err}
	if multi {
		n, ok := r.Targets, r.TargetsOK
		c.Targets, c.TargetsOK = &n, &ok
	}
	return c
}

// Offline is the state topic's last will, and the clean-shutdown message.
type Offline struct {
	Online    bool       `json:"online"`
	Timestamp *time.Time `json:"timestamp,omitempty"`
}

// Outage is published, retained, while an outage is in progress.
type Outage struct {
	StartedAt   time.Time               `json:"started_at"`
	Status      string                  `json:"status"`
	WorstStatus string                  `json:"worst_status"`
	Transitions []outages.TransitionOut `json:"transitions"`
	ClockSynced *bool                   `json:"clock_synced,omitempty"`
}

// NewOutage builds the outage payload.
func NewOutage(o outages.Open) Outage {
	return Outage{StartedAt: o.StartedAt, Status: o.Status, WorstStatus: o.WorstStatus, Transitions: o.Transitions, ClockSynced: o.ClockSynced}
}

// LastOutage is published, retained, once an outage has ended.
type LastOutage struct {
	StartedAt    time.Time               `json:"started_at"`
	EndedAt      time.Time               `json:"ended_at"`
	DurationSecs int64                   `json:"duration_secs"`
	WorstStatus  string                  `json:"worst_status"`
	Transitions  []outages.TransitionOut `json:"transitions"`
	ClockSynced  *bool                   `json:"clock_synced,omitempty"`
	SpansRestart bool                    `json:"spans_restart"`
}

// NewLastOutage builds the last_outage payload.
func NewLastOutage(s outages.Summary) LastOutage {
	return LastOutage{
		StartedAt:    s.StartedAt,
		EndedAt:      s.EndedAt,
		DurationSecs: int64(s.Duration().Round(time.Second) / time.Second),
		WorstStatus:  s.WorstStatus,
		Transitions:  s.Transitions,
		ClockSynced:  s.ClockSynced,
		SpansRestart: s.SpansRestart,
	}
}

// JSON marshals v; the payload types cannot fail to marshal.
func JSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
