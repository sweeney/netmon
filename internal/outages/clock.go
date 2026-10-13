// Copied from github.com/sweeney/ups-mqtt internal/outages.

package outages

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SystemClock is the real Clock.  Mono is Go's monotonic clock, measured from
// when the SystemClock was created; create it once, at process start.
type SystemClock struct {
	start  time.Time
	boot   *time.Duration
	bootID string
}

// NewSystemClock returns a SystemClock whose Mono starts now.
//
// Mono is Go's monotonic clock (CLOCK_MONOTONIC) while the boot offset comes
// from /proc/uptime (CLOCK_BOOTTIME).  They differ only by time spent
// suspended, which a server — and a Raspberry Pi — never is.
func NewSystemClock() *SystemClock {
	c := &SystemClock{start: time.Now(), bootID: bootID()}
	if up, ok := uptime(); ok {
		b := -up
		c.boot = &b
	}
	return c
}

// Now implements Clock.
func (c *SystemClock) Now() time.Time { return time.Now() }

// Mono implements Clock.
func (c *SystemClock) Mono() time.Duration { return time.Since(c.start) }

// BootMono implements Clock.
func (c *SystemClock) BootMono() (time.Duration, bool) {
	if c.boot == nil {
		return 0, false
	}
	return *c.boot, true
}

// Synced implements Clock.
func (c *SystemClock) Synced() (synced, known bool) { return clockSynced() }

// BootID implements Clock.
func (c *SystemClock) BootID() string { return c.bootID }

// parseUptime parses /proc/uptime: seconds since boot, then idle seconds.
func parseUptime(s string) (time.Duration, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty uptime")
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parsing uptime %q: %w", fields[0], err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}
