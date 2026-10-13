package probe

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// PingStats is the outcome of pinging one host.
type PingStats struct {
	Sent, Received int
	RTTs           []float64 // ms
	Err            string    // set when no echo could be sent at all
}

var pingSeq atomic.Uint32

// Ping sends count ICMP echoes to host, one after another, each waiting up to
// timeout. It uses an unprivileged ICMP socket ("ping socket"), which Linux
// allows for groups in net.ipv4.ping_group_range; Debian's default covers
// every group, so no capability is needed.
func Ping(ctx context.Context, host string, count int, timeout time.Duration) PingStats {
	var st PingStats
	ip, err := resolve4(ctx, host)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		st.Err = fmt.Sprintf("icmp socket: %v", err)
		return st
	}
	defer conn.Close() //nolint:errcheck
	dst := &net.UDPAddr{IP: ip}
	buf := make([]byte, 1500)
	for i := 0; i < count && ctx.Err() == nil; i++ {
		seq := int(pingSeq.Add(1) & 0xffff)
		// The kernel rewrites the ID of an unprivileged echo to the socket's
		// port, so replies are matched on sequence number and payload.
		payload := []byte(fmt.Sprintf("netmon %d %d", seq, time.Now().UnixNano()))
		msg := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: seq, Data: payload}}
		wb, err := msg.Marshal(nil)
		if err != nil {
			st.Err = err.Error()
			return st
		}
		start := time.Now()
		if _, err := conn.WriteTo(wb, dst); err != nil {
			st.Err = fmt.Sprintf("sending echo: %v", err)
			st.Sent++
			continue
		}
		st.Sent++
		deadline := start.Add(timeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = conn.SetReadDeadline(deadline)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				break // timeout: lost
			}
			rm, err := icmp.ParseMessage(1, buf[:n])
			if err != nil || rm.Type != ipv4.ICMPTypeEchoReply {
				continue
			}
			if e, ok := rm.Body.(*icmp.Echo); ok && e.Seq == seq && string(e.Data) == string(payload) {
				st.Received++
				st.RTTs = append(st.RTTs, ms(time.Since(start)))
				break
			}
		}
		if i < count-1 {
			// Space the echoes a little so a burst of loss shows as loss.
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if st.Received > 0 {
		st.Err = ""
	}
	return st
}

func resolve4(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4, nil
		}
		return nil, fmt.Errorf("%s: not an IPv4 address", host)
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	return ips[0], nil
}

// DNS looks name up through server directly, bypassing the host's resolver
// (on the Pi that is Tailscale's MagicDNS, which can fail on its own).
func DNS(ctx context.Context, server, name string, timeout time.Duration) Result {
	res := Result{Target: server}
	if server == "" || name == "" {
		res.Err = "no DNS server or name configured"
		return res
	}
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, "udp", server)
		},
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	addrs, err := r.LookupHost(ctx, name)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	if len(addrs) == 0 {
		res.Err = "no addresses"
		return res
	}
	t := ms(time.Since(start))
	res.OK, res.RTTms = true, &t
	return res
}

// RouteFile is where DefaultGateway reads the routing table.
var RouteFile = "/proc/net/route"

// DefaultGateway reads the IPv4 default route's gateway from /proc/net/route.
// It is read every round: the Pi's address and gateway come from DHCP.
func DefaultGateway() (string, error) {
	f, err := os.Open(RouteFile)
	if err != nil {
		return "", fmt.Errorf("no routing table: %w", err)
	}
	defer f.Close() //nolint:errcheck
	return parseRoute(f)
}

// parseRoute finds the lowest-metric default route that is up and has a
// gateway.
func parseRoute(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	best, bestMetric := "", int(^uint(0)>>1)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[0] == "Iface" || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		var flags, metric int
		fmt.Sscanf(f[3], "%x", &flags)  //nolint:errcheck
		fmt.Sscanf(f[6], "%d", &metric) //nolint:errcheck
		const up, gw = 0x1, 0x2
		if flags&up == 0 || flags&gw == 0 {
			continue
		}
		b, err := hex.DecodeString(f[2])
		if err != nil || len(b) != 4 {
			continue
		}
		ip := make(net.IP, 4)
		binary.LittleEndian.PutUint32(ip, binary.BigEndian.Uint32(b))
		if metric < bestMetric {
			best, bestMetric = ip.String(), metric
		}
	}
	if best == "" {
		return "", errors.New("no default route")
	}
	return best, nil
}

// TailscaleCommand runs `tailscale status --json`; a test can replace it.
var TailscaleCommand = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
}

// TailscaleStatus reads Tailscale's view of peer (matched on HostName, or on
// the first label of its DNSName).
func TailscaleStatus(ctx context.Context, peer string) (*Tailscale, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := TailscaleCommand(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	return parseTailscale(out, peer)
}

type tsStatus struct {
	Peer map[string]struct {
		HostName      string
		DNSName       string
		CurAddr       string
		Relay         string
		Online        *bool
		LastHandshake time.Time
	}
}

func parseTailscale(data []byte, peer string) (*Tailscale, error) {
	var st tsStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing tailscale status: %w", err)
	}
	for _, p := range st.Peer {
		short := strings.SplitN(p.DNSName, ".", 2)[0]
		if !strings.EqualFold(p.HostName, peer) && !strings.EqualFold(short, peer) {
			continue
		}
		ts := &Tailscale{Relay: p.Relay, Online: p.Online}
		switch {
		case p.CurAddr != "":
			ts.Path, ts.Endpoint = "direct", p.CurAddr
		case p.Relay != "":
			ts.Path = "relay"
		}
		if !p.LastHandshake.IsZero() && p.LastHandshake.Year() > 1 {
			h := p.LastHandshake.UTC()
			ts.LastHandshake = &h
		}
		return ts, nil
	}
	return nil, fmt.Errorf("tailscale peer %q not found", peer)
}
