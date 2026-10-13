//go:build !linux

// Copied from github.com/sweeney/ups-mqtt internal/outages.

package outages

import "time"

// clockSynced cannot be determined here: there is no portable equivalent of
// adjtimex.  Callers leave clock_synced absent rather than guess.
func clockSynced() (synced, known bool) { return false, false }

// uptime is unknown here, so an outage that ended while no process was
// watching is taken to have ended when this process started.
func uptime() (time.Duration, bool) { return 0, false }

// bootID is unknown here.
func bootID() string { return "" }
