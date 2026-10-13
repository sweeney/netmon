// Package probe runs the network checks: the default gateway, several
// internet hosts, a DNS lookup outside the tailnet, a TCP connect to the MQTT
// broker, and Tailscale's own view of its path to the broker's host.
package probe

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Result is the outcome of one check in one round. Unknown values are nil,
// and so absent from the published payload, rather than zero.
type Result struct {
	OK     bool
	Target string
	// Targets and TargetsOK count hosts, for a check with several.
	Targets   int
	TargetsOK int
	RTTms     *float64 // median round trip (ICMP, DNS) or connect time (TCP)
	LossPct   *float64 // ICMP only
	Err       string   // why it failed, when it did
}

// Tailscale is Tailscale's view of the peer that hosts the broker.
type Tailscale struct {
	Path          string // "direct", "relay" or "" when unknown
	Endpoint      string // the peer's address when direct
	Relay         string // the DERP region in use or preferred
	Online        *bool
	LastHandshake *time.Time
}

// Round is one pass over every check.
type Round struct {
	Gateway   Result
	Internet  Result
	DNS       Result
	Broker    Result
	Tailscale *Tailscale // nil when unavailable
	Link      *Link      // nil when pi-helper's file is absent or unreadable
}

// Config says what to probe.
type Config struct {
	Gateway       string // empty: the default route's gateway, read each round
	Internet      []string
	DNSServer     string // host:port
	DNSName       string
	BrokerAddr    string // host:port
	TailscalePeer string // host name of the broker's host; empty: skip
	PiHelperEnv   string // pi-helper's env file; empty: skip
	Count         int    // ICMP echoes per host per round
	Timeout       time.Duration
}

// Prober runs a Round.
type Prober interface {
	Round(ctx context.Context) Round
}

// System is the real Prober.
type System struct {
	Cfg Config
	// Hooks for tests; nil means the real thing.
	Ping       func(ctx context.Context, host string, count int, timeout time.Duration) PingStats
	GatewayFn  func() (string, error)
	TailscaleF func(ctx context.Context, peer string) (*Tailscale, error)
	LinkFn     func(path string) (*Link, error)
}

// Round implements Prober. The checks run in parallel; a round takes about
// Count × Timeout at worst.
func (s *System) Round(ctx context.Context) Round {
	ping := s.Ping
	if ping == nil {
		ping = Ping
	}
	gwFn := s.GatewayFn
	if gwFn == nil {
		gwFn = DefaultGateway
	}
	tsFn := s.TailscaleF
	if tsFn == nil {
		tsFn = TailscaleStatus
	}
	linkFn := s.LinkFn
	if linkFn == nil {
		linkFn = ReadLink
	}
	count, timeout := s.Cfg.Count, s.Cfg.Timeout
	if count < 1 {
		count = 3
	}
	if timeout <= 0 {
		timeout = time.Second
	}

	var (
		r  Round
		wg sync.WaitGroup
	)
	wg.Add(4)
	go func() {
		defer wg.Done()
		gw := s.Cfg.Gateway
		if gw == "" {
			var err error
			if gw, err = gwFn(); err != nil {
				r.Gateway = Result{Err: err.Error()}
				return
			}
		}
		r.Gateway = pingResult(gw, ping(ctx, gw, count, timeout))
	}()
	go func() {
		defer wg.Done()
		r.Internet = internet(ctx, s.Cfg.Internet, count, timeout, ping)
	}()
	go func() {
		defer wg.Done()
		r.DNS = DNS(ctx, s.Cfg.DNSServer, s.Cfg.DNSName, timeout)
		r.Broker = TCP(ctx, s.Cfg.BrokerAddr, timeout)
	}()
	go func() {
		defer wg.Done()
		if s.Cfg.TailscalePeer == "" {
			return
		}
		if ts, err := tsFn(ctx, s.Cfg.TailscalePeer); err == nil {
			r.Tailscale = ts
		}
	}()
	wg.Wait()
	if s.Cfg.PiHelperEnv != "" {
		if l, err := linkFn(s.Cfg.PiHelperEnv); err == nil {
			r.Link = l
		}
	}
	return r
}

// pingResult turns one host's echoes into a Result: up if any reply came.
func pingResult(host string, st PingStats) Result {
	res := Result{Target: host, OK: st.Received > 0, Err: st.Err}
	if st.Sent > 0 {
		loss := 100 * float64(st.Sent-st.Received) / float64(st.Sent)
		res.LossPct = &loss
	}
	if m, ok := median(st.RTTs); ok {
		res.RTTms = &m
	}
	if res.OK {
		res.Err = ""
	} else if res.Err == "" {
		res.Err = "no reply"
	}
	return res
}

// internet pings every host. It is down only if all of them fail, so one
// host's bad day is not an outage. RTT and loss are over the hosts that
// answered.
func internet(ctx context.Context, hosts []string, count int, timeout time.Duration,
	ping func(context.Context, string, int, time.Duration) PingStats) Result {
	res := Result{Targets: len(hosts)}
	stats := make([]PingStats, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			stats[i] = ping(ctx, h, count, timeout)
		}(i, h)
	}
	wg.Wait()
	var rtts []float64
	var sent, recv int
	for _, st := range stats {
		if st.Received > 0 {
			res.TargetsOK++
			sent += st.Sent
			recv += st.Received
			rtts = append(rtts, st.RTTs...)
		}
	}
	res.OK = res.TargetsOK > 0
	if res.OK {
		loss := 100 * float64(sent-recv) / float64(sent)
		res.LossPct = &loss
		if m, ok := median(rtts); ok {
			res.RTTms = &m
		}
	} else {
		res.Err = "no host answered"
	}
	return res
}

func median(xs []float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return round1(s[n/2]), true
	}
	return round1((s[n/2-1] + s[n/2]) / 2), true
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

func ms(d time.Duration) float64 { return round1(float64(d) / float64(time.Millisecond)) }
