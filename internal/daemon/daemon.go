// Package daemon is netmon's main loop: probe, classify, track outages and
// publish, once per interval.
package daemon

import (
	"context"
	"log"
	"time"

	"github.com/sweeney/netmon/internal/classify"
	"github.com/sweeney/netmon/internal/config"
	"github.com/sweeney/netmon/internal/outages"
	"github.com/sweeney/netmon/internal/payload"
	"github.com/sweeney/netmon/internal/probe"
	"github.com/sweeney/netmon/internal/publisher"
)

// QoS levels. State is telemetry: superseded every round, so not worth
// queueing while the bridge is down. The outage topics are events: the bridge
// queues them and delivers them in order on reconnect.
const (
	StateQOS byte = 0
	EventQOS byte = 1
)

// Daemon holds the loop's state between rounds.
type Daemon struct {
	Cfg     config.Config
	Prober  probe.Prober
	Pub     publisher.Publisher
	Clock   outages.Clock
	Tracker *outages.Tracker

	hyst      classify.Hysteresis
	sinceMono *time.Duration
	restored  bool
	// outageDirty: the retained outage topic needs (re)publishing.
	outageDirty bool
	// clearDue: the retained outage topic may hold a stale outage (from a
	// previous process, or one that just ended) and must be cleared.
	clearDue   bool
	pubFailing bool
}

// New returns a Daemon ready for its first Step.
func New(cfg config.Config, pr probe.Prober, pub publisher.Publisher, clock outages.Clock, tr *outages.Tracker) *Daemon {
	return &Daemon{
		Cfg: cfg, Prober: pr, Pub: pub, Clock: clock, Tracker: tr,
		hyst:     classify.Hysteresis{DownAfter: cfg.Probe.DownAfter, UpAfter: cfg.Probe.UpAfter},
		clearDue: true, // a previous process may have left an outage retained
	}
}

// Topics.
func (d *Daemon) stateTopic() string      { return d.Cfg.Topic("state") }
func (d *Daemon) outageTopic() string     { return d.Cfg.Topic("outage") }
func (d *Daemon) lastOutageTopic() string { return d.Cfg.Topic("last_outage") }

// Status is the smoothed status.
func (d *Daemon) Status() classify.Status { return d.hyst.Current() }

// Reconnected marks everything retained for republishing, after a lost MQTT
// connection: anything published meanwhile failed and was not queued.
func (d *Daemon) Reconnected() {
	if _, open := d.Tracker.Current(); open {
		d.outageDirty = true
	}
}

// Step runs one round.
func (d *Daemon) Step(ctx context.Context) {
	roundAt := d.Clock.Mono()
	r := d.Prober.Round(ctx)
	th := classify.Thresholds{LossPct: d.Cfg.Probe.DegradedLossPct, RTTms: d.Cfg.Probe.DegradedRTTms}
	raw := classify.Raw(r, th)
	prev := d.hyst.Current()
	// The round's time is when it started; the probes took up to a second or
	// two of it.
	st, changed, since := d.hyst.ObserveAt(raw, roundAt)

	if changed {
		d.sinceMono = &since
		if prev == classify.Unknown {
			log.Printf("network status: %s", st)
		} else {
			log.Printf("network status: %s -> %s (%s)", prev, st, describe(r))
		}
		if !d.restored {
			d.restored = true
			d.Tracker.Restore(string(st), st.IsOutage())
		}
		started, ended := d.Tracker.ObserveAt(string(st), st.IsOutage(), since)
		if started {
			log.Printf("network outage started: %s", st)
		}
		if ended {
			log.Printf("network outage ended")
			d.clearDue = true
		}
		if _, open := d.Tracker.Current(); open {
			d.outageDirty = true
		}
	}

	// Events first, so that on a reconnect the bridge delivers the outage
	// before the state that may already say "ok".
	if d.restored {
		d.publishOutage()
		d.publishSummary()
	}
	d.publishState(st, raw, r)
}

func (d *Daemon) publishOutage() {
	if o, open := d.Tracker.Current(); open {
		if !d.outageDirty {
			return
		}
		if d.publish(d.outageTopic(), payload.JSON(payload.NewOutage(o)), EventQOS) {
			d.outageDirty, d.clearDue = false, true
		}
		return
	}
	if d.clearDue && d.publish(d.outageTopic(), nil, EventQOS) {
		d.clearDue = false
	}
}

func (d *Daemon) publishSummary() {
	s, due := d.Tracker.Due()
	if !due {
		return
	}
	if d.publish(d.lastOutageTopic(), payload.JSON(payload.NewLastOutage(s)), EventQOS) {
		kind := "final"
		if !s.Final {
			kind = "interim (clock not synced)"
		}
		log.Printf("published %s last_outage: %s -> %s, %s, worst %s", kind,
			s.StartedAt.Format(time.RFC3339), s.EndedAt.Format(time.RFC3339), s.Duration().Round(time.Second), s.WorstStatus)
		d.Tracker.Published(s)
	}
}

func (d *Daemon) publishState(st, raw classify.Status, r probe.Round) {
	var since *time.Time
	if d.sinceMono != nil {
		t := outages.WallAt(d.Clock, *d.sinceMono)
		since = &t
	}
	var synced *bool
	if s, known := d.Clock.Synced(); known {
		synced = &s
	}
	p := payload.NewState(d.Clock.Now(), synced, string(st), string(raw), since, r)
	d.publish(d.stateTopic(), payload.JSON(p), StateQOS)
}

// publish sends one retained message; a nil payload clears the topic. A run
// of failures is logged at its start and end, not on every message.
func (d *Daemon) publish(topic string, body []byte, qos byte) bool {
	err := d.Pub.Publish(publisher.Message{Topic: topic, Payload: string(body), Retained: true, QOS: qos})
	switch {
	case err != nil && !d.pubFailing:
		log.Printf("publishing failed (logged again when it recovers): %v", err)
		d.pubFailing = true
	case err == nil && d.pubFailing:
		log.Printf("publishing works again")
		d.pubFailing = false
	}
	return err == nil
}

// Offline publishes the clean-shutdown state. It reports whether it got
// through; if not, the caller leaves without a clean disconnect so the broker
// publishes the will instead.
func (d *Daemon) Offline() bool {
	now := d.Clock.Now().UTC()
	return d.publish(d.stateTopic(), payload.JSON(payload.Offline{Online: false, Timestamp: &now}), EventQOS)
}

// WillPayload is the state topic's last will.
func WillPayload() string { return string(payload.JSON(payload.Offline{Online: false})) }

func describe(r probe.Round) string {
	s := "gateway " + okStr(r.Gateway.OK) + ", internet " + okStr(r.Internet.OK) +
		", dns " + okStr(r.DNS.OK)
	if r.Bridge != nil {
		s += ", bridge " + okStr(r.Bridge.OK)
	}
	return s
}

func okStr(ok bool) string {
	if ok {
		return "ok"
	}
	return "failing"
}
