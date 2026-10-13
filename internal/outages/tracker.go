// Package outages follows each network outage from start to end, across
// process restarts and reboots, and reports it once it is over.
//
// An outage is a period in which the smoothed status is lan_down, wan_down or
// tailnet_down. It can change between those partway through; it stays one
// outage, and the summary records every transition and the worst status.
//
// Times follow ups-mqtt's approach (its internal/outages, whose clock code is
// copied here). The wall clock is what gets published, but on a Pi without an
// RTC it is wrong after a power-loss reboot until NTP syncs, and NTP needs the
// network that may be the thing that is down. So every event in this process
// is anchored on the monotonic clock, and its wall time is worked out only
// when it is published: "now, minus how long ago it happened". Once the clock
// has synced that answer is right, however wrong the clock was at the time.
// Until then a summary waits up to SyncWait, then goes out as an interim one
// (clock_synced: false), and a corrected one follows after the sync.
package outages

import (
	"errors"
	"log"
	"time"
)

// DefaultSyncWait is how long a summary waits for the wall clock to sync
// before an interim one is published anyway.
const DefaultSyncWait = 15 * time.Minute

// Clock is the time source. SystemClock is the real one; FakeClock is for
// tests.
type Clock interface {
	Now() time.Time
	Mono() time.Duration
	BootMono() (time.Duration, bool)
	Synced() (synced, known bool)
	BootID() string
}

// Transition is one change of status within an outage. The last one of an
// ended outage is its end, to the status the network recovered to.
type Transition struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"`
	// SinceBoot re-anchors At on the monotonic clock after a process-only
	// restart within the same boot.
	SinceBoot *time.Duration `json:"since_boot_ns,omitempty"`
	// ClockSynced is whether the wall clock was synced when At was taken.
	ClockSynced *bool `json:"clock_synced,omitempty"`

	mono *time.Duration // anchor in this process; nil if restored without one
}

// Record is what is persisted about an outage. It is written only on
// transitions, to spare the SD card.
type Record struct {
	BootID      string       `json:"boot_id,omitempty"`
	Transitions []Transition `json:"transitions"`
	WorstStatus string       `json:"worst_status"`
	// Ended is set once the outage is over, while its summary waits.
	Ended        bool `json:"ended,omitempty"`
	SpansRestart bool `json:"spans_restart,omitempty"`
	// RestartEnd marks an outage that ended while no process was watching:
	// its end is boot time after a reboot, else this process's start.
	RestartEnd bool `json:"restart_end,omitempty"`

	endStatus string // with RestartEnd: the status found on restart
}

// Open describes the outage in progress.
type Open struct {
	StartedAt   time.Time
	Status      string
	WorstStatus string
	ClockSynced *bool
	Transitions []TransitionOut
}

// TransitionOut is a transition as published.
type TransitionOut struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"`
}

// Summary describes an outage that has ended.
type Summary struct {
	StartedAt    time.Time
	EndedAt      time.Time
	WorstStatus  string
	Transitions  []TransitionOut
	ClockSynced  *bool
	SpansRestart bool
	// Final is false for an interim summary published before the clock
	// synced; a corrected one follows.
	Final bool
}

// Duration is EndedAt − StartedAt.
func (s Summary) Duration() time.Duration { return s.EndedAt.Sub(s.StartedAt) }

// Store persists the Record of the outage being tracked: at most one.
type Store interface {
	Load() (*Record, error)
	Save(Record) error
	Remove() error
}

// Tracker follows outages. It is not safe for concurrent use; the main loop
// owns it.
type Tracker struct {
	clock    Clock
	store    Store
	SyncWait time.Duration

	restoreDone bool
	open        *Record
	pending     *pendingSummary
}

type pendingSummary struct {
	rec         Record
	rebooted    *bool
	since       time.Duration // Mono when it became due
	interimSent bool
}

// NewTracker returns a Tracker persisting to store.
func NewTracker(clock Clock, store Store) *Tracker {
	return &Tracker{clock: clock, store: store, SyncWait: DefaultSyncWait}
}

// Restore picks up an outage a previous process left on disk. Call it once
// the status first settles, with that status and whether it is an outage;
// only the first call does anything.
//
//   - An ended outage not yet reported is reported as stored.
//   - An open outage while the network is down now is resumed.
//   - An open outage while the network is up now ended while no process was
//     watching: at boot if the host rebooted since, else at this process's
//     start. Either way it is reported with spans_restart.
func (t *Tracker) Restore(status string, down bool) {
	if t.restoreDone {
		return
	}
	t.restoreDone = true
	rec, err := t.store.Load()
	if errors.Is(err, ErrCorrupt) {
		log.Printf("outage state corrupt, discarding it: %v", err)
		t.remove()
		return
	}
	if err != nil {
		log.Printf("outage state unreadable, ignoring it for now: %v", err)
		return
	}
	if rec == nil || len(rec.Transitions) == 0 {
		return
	}
	rebooted := t.rebootedSince(rec)
	rec.Transitions = append([]Transition(nil), rec.Transitions...)
	for i := range rec.Transitions {
		// A previous process's anchors mean nothing here; within the same
		// boot they can be rebuilt from the time since boot.
		rec.Transitions[i].mono = nil
		if rebooted != nil && !*rebooted {
			rec.Transitions[i].mono = t.reanchor(rec.Transitions[i].SinceBoot)
		}
	}
	started := rec.Transitions[0].At.UTC().Format(time.RFC3339)
	switch {
	case rec.Ended:
		t.pending = &pendingSummary{rec: *rec, rebooted: rebooted, since: t.clock.Mono()}
		log.Printf("network outage started %s ended before this process started and is still unreported", started)
	case down:
		rec.SpansRestart = true
		if rebooted != nil && *rebooted {
			// The record now belongs to this boot; its old anchors do not.
			rec.BootID = t.clock.BootID()
			for i := range rec.Transitions {
				rec.Transitions[i].SinceBoot = nil
			}
		}
		t.open = rec
		t.save(*rec)
		log.Printf("resuming network outage started %s", started)
	default:
		// Not saved: the record stays open on disk, so a restart before the
		// summary goes out reaches the same conclusion.
		rec.Ended, rec.SpansRestart, rec.RestartEnd = true, true, true
		rec.endStatus = status
		t.pending = &pendingSummary{rec: *rec, rebooted: rebooted, since: t.clock.Mono()}
		log.Printf("network outage started %s ended while netmon was not running", started)
	}
}

// Observe records a change of the smoothed status, now. It returns whether
// an outage started and whether one ended with this change.
func (t *Tracker) Observe(status string, isOutage bool) (started, ended bool) {
	return t.ObserveAt(status, isOutage, t.clock.Mono())
}

// ObserveAt is Observe for a change that happened at mono (a reading of the
// Clock's Mono, not in the future): the first round of the run that settled
// the new status.
func (t *Tracker) ObserveAt(status string, isOutage bool, mono time.Duration) (started, ended bool) {
	if now := t.clock.Mono(); mono > now {
		mono = now
	}
	switch {
	case isOutage && t.open == nil:
		if t.pending != nil {
			log.Printf("dropping unpublished summary of the network outage started %s; superseded by a new one",
				t.wallOf(t.pending.rec.Transitions[0]).UTC().Format(time.RFC3339))
			t.pending = nil
		}
		t.open = &Record{BootID: t.clock.BootID(), WorstStatus: status, Transitions: []Transition{t.at(status, mono)}}
		t.save(*t.open)
		return true, false
	case isOutage:
		if tr := t.open.Transitions; tr[len(tr)-1].Status == status {
			return false, false // a resumed outage, still in the same state
		}
		t.open.Transitions = append(t.open.Transitions, t.at(status, mono))
		if worse(status, t.open.WorstStatus) {
			t.open.WorstStatus = status
		}
		t.save(*t.open)
		return false, false
	case t.open != nil:
		rec := *t.open
		t.open = nil
		rec.Transitions = append(rec.Transitions, t.at(status, mono))
		rec.Ended = true
		rec.BootID = t.clock.BootID()
		t.save(rec)
		t.pending = &pendingSummary{rec: rec, since: t.clock.Mono()}
		f := false
		t.pending.rebooted = &f
		return false, true
	}
	return false, false
}

// Current returns the outage in progress, if any.
func (t *Tracker) Current() (Open, bool) {
	if t.open == nil {
		return Open{}, false
	}
	tr := t.open.Transitions
	return Open{
		StartedAt:   t.wallOf(tr[0]).UTC(),
		Status:      tr[len(tr)-1].Status,
		WorstStatus: t.open.WorstStatus,
		ClockSynced: t.allSynced(tr),
		Transitions: t.out(tr),
	}, true
}

// Due returns the summary to publish now, if any. After a successful publish
// call Published with it; until then Due keeps returning it.
func (t *Tracker) Due() (Summary, bool) {
	p := t.pending
	if p == nil {
		return Summary{}, false
	}
	s := t.summarise(p)
	synced, known := t.clock.Synced()
	switch {
	case !known || synced || !t.repairable(p) || t.open != nil:
		s.Final = true
	case !p.interimSent && t.clock.Mono()-p.since >= t.SyncWait:
		s.Final = false
	default:
		return Summary{}, false
	}
	return s, true
}

// Published records that s went out. A final summary is done with, and the
// state file removed unless a new outage owns it.
func (t *Tracker) Published(s Summary) {
	if t.pending == nil {
		return
	}
	if !s.Final {
		t.pending.interimSent = true
		return
	}
	t.pending = nil
	if t.open == nil {
		t.remove()
	}
}

func (t *Tracker) summarise(p *pendingSummary) Summary {
	tr := p.rec.Transitions
	start := t.wallOf(tr[0])
	var end time.Time
	outs := t.out(tr)
	if p.rec.RestartEnd {
		end = t.wallAt(0)
		if boot, ok := t.clock.BootMono(); ok {
			bootWall := t.wallAt(boot)
			if (p.rebooted != nil && *p.rebooted) || (p.rebooted == nil && bootWall.After(start)) {
				end = bootWall
			}
		}
		if end.Before(outs[len(outs)-1].At) {
			end = outs[len(outs)-1].At
		}
		outs = append(outs, TransitionOut{At: end.UTC(), Status: p.rec.endStatus})
	} else {
		end = outs[len(outs)-1].At
	}
	if end.Before(start) {
		end = start
	}
	var cs *bool
	if synced, known := t.clock.Synced(); known {
		ok := synced && isTrue(t.allSynced(tr))
		cs = &ok
	}
	return Summary{
		StartedAt:    start.UTC(),
		EndedAt:      end.UTC(),
		WorstStatus:  p.rec.WorstStatus,
		Transitions:  outs,
		ClockSynced:  cs,
		SpansRestart: p.rec.SpansRestart,
	}
}

// repairable reports whether the end is anchored on this boot's monotonic
// clock, so that a later clock sync corrects it: an end seen by this process,
// or one taken to be boot time or this process's start. Whether the start is
// right as well is what clock_synced says.
func (t *Tracker) repairable(p *pendingSummary) bool {
	if p.rec.RestartEnd {
		return true
	}
	tr := p.rec.Transitions
	return tr[len(tr)-1].mono != nil
}

func (t *Tracker) at(status string, m time.Duration) Transition {
	return Transition{At: t.wallAt(m), Status: status, SinceBoot: t.sinceBoot(&m), ClockSynced: t.syncedNow(), mono: &m}
}

func (t *Tracker) out(tr []Transition) []TransitionOut {
	o := make([]TransitionOut, len(tr))
	for i, x := range tr {
		o[i] = TransitionOut{At: t.wallOf(x).UTC(), Status: x.Status}
	}
	return o
}

// wallOf is a transition's wall time, as best known now.
func (t *Tracker) wallOf(x Transition) time.Time {
	if x.mono != nil {
		return t.wallAt(*x.mono)
	}
	return x.At
}

// allSynced reports whether every time is right now: those anchored here once
// the clock syncs, those restored without an anchor only if the clock was
// synced when they were taken.
func (t *Tracker) allSynced(tr []Transition) *bool {
	synced, known := t.clock.Synced()
	if !known {
		return nil
	}
	ok := synced
	for _, x := range tr {
		if x.mono == nil && !isTrue(x.ClockSynced) {
			ok = false
		}
	}
	return &ok
}

func (t *Tracker) wallAt(m time.Duration) time.Time {
	return t.clock.Now().Add(m - t.clock.Mono())
}

func (t *Tracker) rebootedSince(rec *Record) *bool {
	now := t.clock.BootID()
	if now == "" || rec.BootID == "" {
		return nil
	}
	r := now != rec.BootID
	return &r
}

func (t *Tracker) reanchor(sinceBoot *time.Duration) *time.Duration {
	boot, ok := t.clock.BootMono()
	if !ok || sinceBoot == nil {
		return nil
	}
	m := boot + *sinceBoot
	return &m
}

func (t *Tracker) sinceBoot(m *time.Duration) *time.Duration {
	boot, ok := t.clock.BootMono()
	if !ok || m == nil {
		return nil
	}
	d := *m - boot
	return &d
}

func (t *Tracker) syncedNow() *bool {
	synced, known := t.clock.Synced()
	if !known {
		return nil
	}
	return &synced
}

func (t *Tracker) save(rec Record) {
	if err := t.store.Save(rec); err != nil {
		log.Printf("saving outage state: %v", err)
	}
}

func (t *Tracker) remove() {
	if err := t.store.Remove(); err != nil {
		log.Printf("removing outage state: %v", err)
	}
}

// worse orders outage statuses; it is a local copy of classify's ordering so
// this package depends only on strings.
func worse(a, b string) bool { return rank[a] > rank[b] }

var rank = map[string]int{"tailnet_down": 1, "wan_down": 2, "lan_down": 3}

func isTrue(b *bool) bool { return b != nil && *b }
