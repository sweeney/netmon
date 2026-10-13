package outages

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)

func dur(d time.Duration) *time.Duration { return &d }

// newClock is a synced clock 1 h after boot.
func newClock() *FakeClock {
	return &FakeClock{Wall: t0, M: 0, Boot: dur(-time.Hour), IsSynced: true, SyncKnown: true, BootIDValue: "boot-1"}
}

func mustDue(t *testing.T, tr *Tracker) Summary {
	t.Helper()
	s, ok := tr.Due()
	if !ok {
		t.Fatal("no summary due")
	}
	return s
}

func TestOutageLifecycle(t *testing.T) {
	c, st := newClock(), &MemoryStore{}
	tr := NewTracker(c, st)
	tr.Restore("ok", false)

	if started, _ := tr.Observe("wan_down", true); !started {
		t.Fatal("outage not started")
	}
	c.Advance(2 * time.Minute)
	tr.Observe("lan_down", true)
	c.Advance(time.Minute)
	tr.Observe("wan_down", true)
	o, open := tr.Current()
	if !open || o.Status != "wan_down" || o.WorstStatus != "lan_down" || len(o.Transitions) != 3 || !o.StartedAt.Equal(t0) {
		t.Fatalf("current: %+v", o)
	}
	c.Advance(time.Minute)
	if _, ended := tr.Observe("ok", false); !ended {
		t.Fatal("outage not ended")
	}
	if _, open := tr.Current(); open {
		t.Error("still open")
	}
	s := mustDue(t, tr)
	if !s.Final || s.Duration() != 4*time.Minute || s.WorstStatus != "lan_down" || s.SpansRestart {
		t.Errorf("summary: %+v", s)
	}
	want := []string{"wan_down", "lan_down", "wan_down", "ok"}
	for i, w := range want {
		if s.Transitions[i].Status != w {
			t.Errorf("transition %d: %s, want %s", i, s.Transitions[i].Status, w)
		}
	}
	if s.ClockSynced == nil || !*s.ClockSynced {
		t.Error("clock_synced should be true")
	}
	if st.Saves != 4 {
		t.Errorf("saves: %d, want one per transition", st.Saves)
	}
	tr.Published(s)
	if _, ok := tr.Due(); ok {
		t.Error("still due after publish")
	}
	if st.Rec != nil {
		t.Error("state file not removed")
	}
}

func TestSameStatusTwiceIsNotATransition(t *testing.T) {
	c, st := newClock(), &MemoryStore{}
	tr := NewTracker(c, st)
	tr.Restore("wan_down", true)
	tr.Observe("wan_down", true)
	tr.Observe("wan_down", true)
	if o, _ := tr.Current(); len(o.Transitions) != 1 {
		t.Errorf("transitions: %d", len(o.Transitions))
	}
}

func TestUnsyncedClockInterimThenFinal(t *testing.T) {
	c := newClock()
	c.IsSynced = false
	tr := NewTracker(c, &MemoryStore{})
	tr.Restore("ok", false)
	tr.Observe("wan_down", true)
	c.Advance(5 * time.Minute)
	tr.Observe("ok", false)
	if _, ok := tr.Due(); ok {
		t.Fatal("summary due before the sync wait")
	}
	c.Advance(DefaultSyncWait)
	s := mustDue(t, tr)
	if s.Final || s.ClockSynced == nil || *s.ClockSynced {
		t.Fatalf("interim: %+v", s)
	}
	tr.Published(s)
	if _, ok := tr.Due(); ok {
		t.Fatal("a second interim")
	}
	// NTP steps the clock forward 3 days: the times move with it.
	c.Step(72 * time.Hour)
	s = mustDue(t, tr)
	if !s.Final || !*s.ClockSynced || !s.StartedAt.Equal(t0.Add(72*time.Hour)) || s.Duration() != 5*time.Minute {
		t.Errorf("corrected: %+v", s)
	}
}

func TestNewOutageSupersedesUnpublishedSummary(t *testing.T) {
	c := newClock()
	tr := NewTracker(c, &MemoryStore{})
	tr.Restore("ok", false)
	tr.Observe("wan_down", true)
	c.Advance(time.Minute)
	tr.Observe("ok", false) // summary never published
	c.Advance(time.Minute)
	tr.Observe("lan_down", true)
	if _, ok := tr.Due(); ok {
		t.Error("the superseded summary is still due")
	}
}

// restart builds a new Tracker over the same store, as a new process does.
func restart(c *FakeClock, st Store, rebooted bool) (*Tracker, *FakeClock) {
	n := *c
	n.M = 0
	if rebooted {
		n.BootIDValue = c.BootIDValue + "x"
		n.Boot = dur(-30 * time.Second) // up 30 s
	} else {
		b := *c.Boot - c.M // same boot: boot is further back in this process's terms
		n.Boot = &b
	}
	return NewTracker(&n, st), &n
}

func TestRestartMidOutageSameBootResumes(t *testing.T) {
	c, st := newClock(), &MemoryStore{}
	tr := NewTracker(c, st)
	tr.Restore("ok", false)
	tr.Observe("wan_down", true)
	c.Advance(3 * time.Minute)

	tr2, c2 := restart(c, st, false)
	tr2.Restore("wan_down", true)
	o, open := tr2.Current()
	if !open || !o.StartedAt.Equal(t0) {
		t.Fatalf("resumed: %+v", o)
	}
	c2.Advance(2 * time.Minute)
	tr2.Observe("ok", false)
	s := mustDue(t, tr2)
	if !s.SpansRestart || s.Duration() != 5*time.Minute || !s.Final {
		t.Errorf("summary: %+v", s)
	}
}

func TestRestartAfterOutageEndedWhileNotRunning(t *testing.T) {
	c, st := newClock(), &MemoryStore{}
	tr := NewTracker(c, st)
	tr.Restore("ok", false)
	tr.Observe("lan_down", true)
	c.Advance(10 * time.Minute)

	// Process-only restart, network up again: ended at this process's start.
	tr2, c2 := restart(c, st, false)
	c2.Advance(20 * time.Second) // status settles a couple of rounds in
	tr2.Restore("ok", false)
	s := mustDue(t, tr2)
	if !s.SpansRestart || !s.EndedAt.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("process restart: %+v", s)
	}
	if last := s.Transitions[len(s.Transitions)-1]; last.Status != "ok" {
		t.Errorf("last transition: %+v", last)
	}
	if st.Rec == nil {
		t.Fatal("record removed before the summary went out")
	}
	tr2.Published(s)
	if st.Rec != nil {
		t.Error("record kept after the summary went out")
	}
}

func TestRebootAfterOutageEndsAtBoot(t *testing.T) {
	c, st := newClock(), &MemoryStore{}
	tr := NewTracker(c, st)
	tr.Restore("ok", false)
	tr.Observe("lan_down", true)
	c.Advance(10 * time.Minute)
	// Power cut: the Pi goes down at +10 min and boots 20 min later (at
	// +30 min), its wall clock restored from the last save (+10 min) and
	// unsynced.
	tr2, c2 := restart(c, st, true)
	c2.Wall = t0.Add(10*time.Minute + 30*time.Second) // boot was 30 s ago, as it believes
	c2.IsSynced = false
	tr2.Restore("ok", false)
	if _, ok := tr2.Due(); ok {
		t.Fatal("due before the clock synced: the end (boot time) is still wrong")
	}
	c2.Advance(time.Minute)
	c2.Step(20 * time.Minute) // NTP: the real time is 20 min later than the restored clock
	s := mustDue(t, tr2)
	if !s.Final || !s.SpansRestart || !s.EndedAt.Equal(t0.Add(30*time.Minute)) || !s.StartedAt.Equal(t0) {
		t.Errorf("summary: %+v", s)
	}
	if s.ClockSynced == nil || !*s.ClockSynced {
		t.Error("clock_synced should be true: the start was taken on a synced clock")
	}
}

func TestRebootDuringOutageResumesWithoutAnchor(t *testing.T) {
	c, st := newClock(), &MemoryStore{}
	tr := NewTracker(c, st)
	tr.Restore("ok", false)
	tr.Observe("wan_down", true)
	tr2, c2 := restart(c, st, true)
	tr2.Restore("wan_down", true)
	if st.Rec.BootID != c2.BootIDValue {
		t.Error("record not moved to the new boot")
	}
	for _, x := range st.Rec.Transitions {
		if x.SinceBoot != nil {
			t.Error("old boot's anchor kept")
		}
	}
	c2.Advance(time.Minute)
	tr2.Observe("ok", false)
	s := mustDue(t, tr2)
	if !s.SpansRestart || !s.StartedAt.Equal(t0) {
		t.Errorf("summary: %+v", s)
	}
}

func TestEndedButUnpublishedSurvivesRestart(t *testing.T) {
	c, st := newClock(), &MemoryStore{}
	tr := NewTracker(c, st)
	tr.Restore("ok", false)
	tr.Observe("tailnet_down", true)
	c.Advance(time.Minute)
	tr.Observe("ok", false)

	tr2, _ := restart(c, st, false)
	tr2.Restore("ok", false)
	s := mustDue(t, tr2)
	if s.Duration() != time.Minute || s.WorstStatus != "tailnet_down" || s.SpansRestart {
		t.Errorf("summary: %+v", s)
	}
}

func TestRestoreOnlyOnce(t *testing.T) {
	st := &MemoryStore{Rec: &Record{Transitions: []Transition{{At: t0, Status: "wan_down"}}, WorstStatus: "wan_down"}}
	tr := NewTracker(newClock(), st)
	tr.Restore("wan_down", true)
	tr.Restore("ok", false)
	if _, open := tr.Current(); !open {
		t.Error("second Restore changed things")
	}
}

func TestFileStore(t *testing.T) {
	dir := t.TempDir()
	s := &FileStore{Path: filepath.Join(dir, StateFileName)}
	if r, err := s.Load(); r != nil || err != nil {
		t.Fatalf("empty: %v %v", r, err)
	}
	rec := Record{BootID: "b", WorstStatus: "lan_down", Transitions: []Transition{{At: t0, Status: "lan_down", SinceBoot: dur(time.Hour)}}}
	if err := s.Save(rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.WorstStatus != "lan_down" || *got.Transitions[0].SinceBoot != time.Hour || !got.Transitions[0].At.Equal(t0) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if err := os.WriteFile(s.Path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Error("corrupt: want an error")
	}
	tr := NewTracker(newClock(), s)
	tr.Restore("ok", false) // discards the corrupt file
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Error("corrupt file kept")
	}
	if err := s.Remove(); err != nil {
		t.Error(err)
	}
}
