// Copied from github.com/sweeney/ups-mqtt internal/outages.

package outages

import (
	"os"
	"strings"
	"syscall"
	"time"
)

// staUnsync is the kernel's "clock not synchronised" status bit.  NTP
// daemons (systemd-timesyncd, chrony, ntpd) clear it once they have
// disciplined the clock.
const staUnsync = 0x0040

// clockSynced asks the kernel, via a read-only adjtimex, whether the clock
// is synchronised.  It needs no privilege.
func clockSynced() (synced, known bool) {
	var tx syscall.Timex
	if _, err := syscall.Adjtimex(&tx); err != nil {
		return false, false
	}
	return tx.Status&staUnsync == 0, true
}

// uptime is the time since boot, from /proc/uptime.
func uptime() (time.Duration, bool) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	d, err := parseUptime(string(data))
	return d, err == nil
}

// bootID is the kernel's random ID for this boot, or "" if unreadable.
func bootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
