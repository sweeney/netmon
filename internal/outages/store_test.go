package outages

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestFileStoreErrors(t *testing.T) {
	dir := t.TempDir()
	missing := &FileStore{Path: filepath.Join(dir, "no-such-dir", StateFileName)}
	rec := Record{Transitions: []Transition{{At: t0, Status: "lan_down"}}}
	if err := missing.Save(State{Open: &rec}); err == nil {
		t.Error("save into a missing directory: want an error")
	}
	if _, err := os.Stat(missing.Path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temporary file left behind")
	}

	// The path is a directory: reading it is an I/O error, not corruption,
	// so the tracker must not discard it.
	asDir := &FileStore{Path: filepath.Join(dir, "a-dir")}
	if err := os.MkdirAll(filepath.Join(asDir.Path, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := asDir.Load(); err == nil || errors.Is(err, ErrCorrupt) {
		t.Errorf("load a directory: got %v, want a non-corrupt error", err)
	}
	if err := asDir.Save(State{Open: &rec}); err == nil {
		t.Error("rename over a non-empty directory: want an error")
	}
	if _, err := os.Stat(asDir.Path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temporary file left behind after a failed rename")
	}
	if err := asDir.Remove(); err == nil {
		t.Error("remove a non-empty directory: want an error")
	}

	none := &FileStore{Path: filepath.Join(dir, StateFileName)}
	if err := none.Remove(); err != nil {
		t.Errorf("removing a file that is not there: %v", err)
	}
}

func TestStateValid(t *testing.T) {
	start := Transition{At: t0, Status: "lan_down"}
	cases := map[string]State{
		"empty":                {},
		"open without a start": {Open: &Record{}},
		"zero start time":      {Open: &Record{Transitions: []Transition{{Status: "lan_down"}}}},
		"pending without end":  {Pending: []Record{{Transitions: []Transition{start}}}},
	}
	for name, st := range cases {
		if err := st.valid(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := State{Open: &Record{Transitions: []Transition{start}}, Pending: []Record{{Transitions: []Transition{start, {At: t0.Add(time.Minute), Status: "ok"}}}}}
	if err := ok.valid(); err != nil {
		t.Errorf("valid state rejected: %v", err)
	}
}

func TestNopStore(t *testing.T) {
	var s NopStore
	if st, err := s.Load(); st != nil || err != nil {
		t.Errorf("load: %v %v", st, err)
	}
	if s.Save(State{}) != nil || s.Remove() != nil {
		t.Error("NopStore failed")
	}
}

func TestParseUptime(t *testing.T) {
	if d, err := parseUptime("12345.67 54321.00\n"); err != nil || d != 12345670*time.Millisecond {
		t.Errorf("got %s, %v", d, err)
	}
	for _, bad := range []string{"", "soon 1.0"} {
		if _, err := parseUptime(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestSystemClock(t *testing.T) {
	c := NewSystemClock()
	if d := time.Since(c.Now()); d < 0 || d > time.Second {
		t.Errorf("Now is %s off", d)
	}
	if c.Mono() < 0 {
		t.Error("Mono went backwards")
	}
	boot, bootKnown := c.BootMono()
	_, syncKnown := c.Synced()
	if runtime.GOOS != "linux" {
		if bootKnown || syncKnown || c.BootID() != "" {
			t.Errorf("off Linux nothing is known: boot %v sync %v id %q", bootKnown, syncKnown, c.BootID())
		}
		return
	}
	if !bootKnown || boot >= 0 || !syncKnown || len(c.BootID()) != 36 {
		t.Errorf("on Linux: boot %s %v, sync known %v, boot id %q", boot, bootKnown, syncKnown, c.BootID())
	}
}
