// Package outages follows each network outage from start to end, across
// process restarts and reboots, and reports it once it is over.
//
// An outage is a period in which the smoothed status is lan_down, wan_down or
// bridge_down. It can change between those partway through; it stays one
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

// Record is what is persisted about one outage. It is written only on
// transitions, to spare the SD card.
type Record struct {
	// BootID is the boot whose monotonic clock the SinceBoot values are on.
	BootID      string       `json:"boot_id,omitempty"`
	Transitions []Transition `json:"transitions"`
	WorstStatus string       `json:"worst_status"`
	// SpansRestart: a process restart or reboot happened during the outage.
	SpansRestart bool `json:"spans_restart,omitempty"`
	// InterimSent: an interim summary went out; only the corrected one is due.
	InterimSent bool `json:"interim_sent,omitempty"`
}

// State is everything persisted: the outage in progress, if any, and the
// ended ones whose summaries have not gone out yet, oldest first. A new
// outage can start while an older summary still waits for the clock to sync
// or for the broker.
type State struct {
	Open    *Record  `json:"open,omitempty"`
	Pending []Record `json:"pending,omitempty"`
}

// MaxPending bounds the queue of unpublished summaries; past it the oldest is
// dropped (and logged).
const MaxPending = 10

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

// Store persists the State.
type Store interface {
	Load() (*State, error)
	Save(State) error
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
	pending     []*pendingSummary
}

type pendingSummary struct {
	rec   Record
	since time.Duration // Mono when it became due, for SyncWait
}

// NewTracker returns a Tracker persisting to store.
func NewTracker(clock Clock, store Store) *Tracker {
	return &Tracker{clock: clock, store: store, SyncWait: DefaultSyncWait}
}

// Restore picks up what a previous process left on disk. Call it once the
// status first settles, with that status and whether it is an outage; only
// the first call does anything.
//
//   - Summaries not yet published are queued again.
//   - An open outage while the network is down now is resumed.
//   - An open outage while the network is up now ended while no process was
//     watching: at boot if the host rebooted since, else at this process's
//     start. That end is recorded once, so a further restart does not move it.
//
// Either way the outage is reported with spans_restart.
func (t *Tracker) Restore(status string, down bool) {
	if t.restoreDone {
		return
	}
	t.restoreDone = true
	st, err := t.store.Load()
	if errors.Is(err, ErrCorrupt) {
		log.Printf("outage state corrupt, discarding it: %v", err)
		t.remove()
		return
	}
	if err != nil {
		log.Printf("outage state unreadable, ignoring it for now: %v", err)
		return
	}
	if st == nil {
		return
	}
	for _, rec := range st.Pending {
		rec := rec
		t.adopt(&rec)
		t.pending = append(t.pending, &pendingSummary{rec: rec, since: t.clock.Mono()})
		log.Printf("network outage started %s ended before this process started and is still unreported", startedStr(rec))
	}
	if rec := st.Open; rec != nil {
		rebooted := t.adopt(rec)
		rec.SpansRestart = true
		if down {
			t.open = rec
			log.Printf("resuming network outage started %s", startedStr(*rec))
		} else {
			end := time.Duration(0) // this process's start
			if boot, ok := t.clock.BootMono(); ok {
				startWall := t.wallOf(rec.Transitions[0])
				if isTrue(rebooted) || rebooted == nil && t.wallAt(boot).After(startWall) {
					end = boot
				}
			}
			rec.Transitions = append(rec.Transitions, t.at(status, end))
			t.pending = append(t.pending, &pendingSummary{rec: *rec, since: t.clock.Mono()})
			log.Printf("network outage started %s ended while netmon was not running", startedStr(*rec))
		}
	}
	t.trimPending()
	t.save()
}

// adopt makes a stored record this boot's: anchors from the same boot are
// rebuilt from their time since boot; those from another boot are dropped,
// and the record moves to this boot. It returns whether the host rebooted
// since the record was written (nil if unknown).
func (t *Tracker) adopt(rec *Record) *bool {
	rebooted := t.rebootedSince(rec)
	rec.Transitions = append([]Transition(nil), rec.Transitions...)
	for i := range rec.Transitions {
		x := &rec.Transitions[i]
		x.mono = nil
		if rebooted != nil && !*rebooted {
			x.mono = t.reanchor(x.SinceBoot)
		} else {
			x.SinceBoot = nil
		}
	}
	rec.BootID = t.clock.BootID()
	return rebooted
}

func startedStr(rec Record) string { return rec.Transitions[0].At.UTC().Format(time.RFC3339) }

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
		t.open = &Record{BootID: t.clock.BootID(), WorstStatus: status, Transitions: []Transition{t.at(status, mono)}}
		t.save()
		return true, false
	case isOutage:
		if tr := t.open.Transitions; tr[len(tr)-1].Status == status {
			return false, false // a resumed outage, still in the same state
		}
		t.open.Transitions = append(t.open.Transitions, t.at(status, mono))
		if worse(status, t.open.WorstStatus) {
			t.open.WorstStatus = status
		}
		t.save()
		return false, false
	case t.open != nil:
		rec := *t.open
		t.open = nil
		rec.Transitions = append(rec.Transitions, t.at(status, mono))
		t.pending = append(t.pending, &pendingSummary{rec: rec, since: t.clock.Mono()})
		t.trimPending()
		t.save()
		return false, true
	}
	return false, false
}

func (t *Tracker) trimPending() {
	for len(t.pending) > MaxPending {
		log.Printf("dropping unpublished summary of the network outage started %s: %d are queued",
			t.wallOf(t.pending[0].rec.Transitions[0]).UTC().Format(time.RFC3339), len(t.pending))
		t.pending = t.pending[1:]
	}
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

// Due returns the summary to publish now, if any: the oldest one queued.
// After a successful publish call Published with it; until then Due keeps
// returning it.
//
// A summary is final once the wall clock is synced, or sync cannot be
// determined, or its end cannot be corrected by a sync anyway. Until then it
// waits up to SyncWait, after which one interim summary goes out marked
// clock_synced: false, and the corrected one follows the sync.
func (t *Tracker) Due() (Summary, bool) {
	if len(t.pending) == 0 {
		return Summary{}, false
	}
	p := t.pending[0]
	s := t.summarise(p.rec)
	synced, known := t.clock.Synced()
	switch {
	case !known || synced || !t.repairable(p.rec):
		s.Final = true
	case !p.rec.InterimSent && t.clock.Mono()-p.since >= t.SyncWait:
		s.Final = false
	default:
		return Summary{}, false
	}
	return s, true
}

// Published records that s, the summary Due returned, went out.
func (t *Tracker) Published(s Summary) {
	if len(t.pending) == 0 {
		return
	}
	if !s.Final {
		t.pending[0].rec.InterimSent = true
	} else {
		t.pending = t.pending[1:]
	}
	t.save()
}

func (t *Tracker) summarise(rec Record) Summary {
	tr := rec.Transitions
	outs := t.out(tr)
	start, end := outs[0].At, outs[len(outs)-1].At
	if end.Before(start) {
		end = start // a clock that was badly wrong at the start
	}
	var cs *bool
	if synced, known := t.clock.Synced(); known {
		ok := synced && isTrue(t.allSynced(tr))
		cs = &ok
	}
	return Summary{
		StartedAt:    start,
		EndedAt:      end,
		WorstStatus:  rec.WorstStatus,
		Transitions:  outs,
		ClockSynced:  cs,
		SpansRestart: rec.SpansRestart,
	}
}

// repairable reports whether the end is anchored on this boot's monotonic
// clock, so that a later clock sync corrects it. Whether the start is right as
// well is what clock_synced says.
func (t *Tracker) repairable(rec Record) bool {
	tr := rec.Transitions
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

func (t *Tracker) wallAt(m time.Duration) time.Time { return WallAt(t.clock, m) }

// WallAt is the wall time of monotonic instant m, as best known now. It is
// worked out afresh each time, so a clock that syncs later corrects it, and
// rounded to the second: Now and Mono drift apart by microseconds (NTP slews
// the wall clock), and an unrounded time would differ in every payload.
func WallAt(c Clock, m time.Duration) time.Time {
	return c.Now().Add(m - c.Mono()).Round(time.Second)
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

// save writes the whole state, or removes the file when there is nothing to
// keep.
func (t *Tracker) save() {
	if t.open == nil && len(t.pending) == 0 {
		t.remove()
		return
	}
	st := State{Open: t.open}
	for _, p := range t.pending {
		st.Pending = append(st.Pending, p.rec)
	}
	if err := t.store.Save(st); err != nil {
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

// tailnet_down is bridge_down's name before 2026-10-14; a state file from then
// may still hold it.
var rank = map[string]int{"tailnet_down": 1, "bridge_down": 1, "wan_down": 2, "lan_down": 3}

func isTrue(b *bool) bool { return b != nil && *b }
