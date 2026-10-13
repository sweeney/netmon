package outages

import (
	"testing"
	"time"
)

// The wall time of a fixed instant must not wobble from one publish to the
// next as the wall and monotonic clocks drift apart by microseconds (seen on
// the Pi: status_since differed in its last digits in every state payload),
// but a real step, as an NTP sync makes, must still correct it.
func TestWallAtStableUnderDrift(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 6, 12, 8_150_150, time.UTC)
	c := &FakeClock{Wall: at, M: time.Hour}
	want := WallAt(c, time.Hour)
	if !want.Equal(time.Date(2026, 10, 14, 10, 6, 12, 0, time.UTC)) {
		t.Fatalf("not rounded to the second: %s", want.Format(time.RFC3339Nano))
	}
	for _, drift := range []time.Duration{-900 * time.Nanosecond, 700 * time.Nanosecond, 3 * time.Microsecond, 40 * time.Millisecond} {
		c.Wall = at.Add(10*time.Second + drift)
		c.M = time.Hour + 10*time.Second
		if got := WallAt(c, time.Hour); !got.Equal(want) {
			t.Errorf("drift %s: got %s, want %s", drift, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
		}
	}
	c.Wall = c.Wall.Add(5 * time.Minute) // NTP step
	if got := WallAt(c, time.Hour); !got.Equal(want.Add(5 * time.Minute)) {
		t.Errorf("after a step: got %s, want %s", got, want.Add(5*time.Minute))
	}
}
