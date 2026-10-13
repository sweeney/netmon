package probe

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Link is the Pi's local link as pi-helper (github.com/sweeney/pi-helper)
// sees it. It is reported only; the status comes from the checks.
type Link struct {
	Status     string // connected, disconnected, connecting
	Type       string // wifi, ethernet
	IP         string
	WifiStatus string // connected, disconnected, no-hardware
	WifiSSID   string
}

// ReadLink reads pi-helper's env file. It returns nil, nil when the file
// does not exist: pi-helper is optional.
func ReadLink(path string) (*Link, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	return parseLink(f)
}

// parseLink reads KEY=VALUE lines, as pi-helper writes them (unquoted).
func parseLink(r io.Reader) (*Link, error) {
	l := &Link{}
	fields := map[string]*string{
		"NETWORK_STATUS":      &l.Status,
		"NETWORK_TYPE":        &l.Type,
		"NETWORK_IP":          &l.IP,
		"NETWORK_WIFI_STATUS": &l.WifiStatus,
		"NETWORK_WIFI_SSID":   &l.WifiSSID,
	}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if p, known := fields[strings.TrimSpace(k)]; ok && known {
			*p = strings.TrimSpace(v)
		}
	}
	return l, sc.Err()
}
