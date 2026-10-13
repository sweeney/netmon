package outages

import "time"

// FakeClock is a Clock for tests.  Advance moves both clocks, as time
// passing does; Step moves only the wall clock, as an NTP sync does.
type FakeClock struct {
	Wall time.Time
	M    time.Duration
	// Boot is the Mono reading at boot; nil when unknown.
	Boot *time.Duration
	// IsSynced and SyncKnown are what Synced returns.
	IsSynced  bool
	SyncKnown bool
	// BootIDValue is what BootID returns; empty when unknown.
	BootIDValue string
}

// Now implements Clock.
func (c *FakeClock) Now() time.Time { return c.Wall }

// Mono implements Clock.
func (c *FakeClock) Mono() time.Duration { return c.M }

// BootMono implements Clock.
func (c *FakeClock) BootMono() (time.Duration, bool) {
	if c.Boot == nil {
		return 0, false
	}
	return *c.Boot, true
}

// Synced implements Clock.
func (c *FakeClock) Synced() (synced, known bool) { return c.IsSynced, c.SyncKnown }

// BootID implements Clock.
func (c *FakeClock) BootID() string { return c.BootIDValue }

// Advance lets d pass.
func (c *FakeClock) Advance(d time.Duration) {
	c.Wall = c.Wall.Add(d)
	c.M += d
}

// Step jumps the wall clock by d without time passing, and marks it synced.
func (c *FakeClock) Step(d time.Duration) {
	c.Wall = c.Wall.Add(d)
	c.IsSynced, c.SyncKnown = true, true
}

// MemoryStore is a Store for tests.  It outlives a Tracker, the way the state
// file outlives a process.
type MemoryStore struct {
	Rec *Record
	// Saves counts writes, to check that only transitions are written.
	Saves   int
	Removes int
	// SaveErr, when set, is returned by Save.
	SaveErr error
}

// Load implements Store.
func (s *MemoryStore) Load() (*Record, error) {
	if s.Rec == nil {
		return nil, nil
	}
	r := *s.Rec
	return &r, nil
}

// Save implements Store.
func (s *MemoryStore) Save(r Record) error {
	if s.SaveErr != nil {
		return s.SaveErr
	}
	s.Saves++
	s.Rec = &r
	return nil
}

// Remove implements Store.
func (s *MemoryStore) Remove() error {
	s.Removes++
	s.Rec = nil
	return nil
}
