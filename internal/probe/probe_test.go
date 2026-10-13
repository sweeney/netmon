package probe

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// Captured on pi3b-schoolhouse, 2026-10-13.
const piRoute = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0100A8C0	0003	0	0	100	00000000	0	0	0
eth0	0000A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
`

func TestParseRoute(t *testing.T) {
	gw, err := parseRoute(strings.NewReader(piRoute))
	if err != nil || gw != "192.168.0.1" {
		t.Fatalf("got %q, %v", gw, err)
	}
	two := piRoute + "wlan0	00000000	FE00A8C0	0003	0	0	600	00000000	0	0	0\n"
	if gw, _ := parseRoute(strings.NewReader(two)); gw != "192.168.0.1" {
		t.Errorf("lowest metric: got %q", gw)
	}
	down := strings.Replace(piRoute, "0003", "0002", 1)
	if _, err := parseRoute(strings.NewReader(down)); err == nil {
		t.Error("a route that is not up was used")
	}
	if _, err := parseRoute(strings.NewReader("Iface\tDestination\n")); err == nil {
		t.Error("no default route: want an error")
	}
}

// Trimmed from `tailscale status --json` on the Pi (addresses anonymised).
const tsJSON = `{"BackendState":"Running","Peer":{
 "nodekey:a":{"HostName":"garibaldi","DNSName":"garibaldi.example-tailnet.ts.net.","CurAddr":"203.0.113.10:51826","Relay":"lhr","Online":true,"LastHandshake":"2026-10-13T21:36:00.837994522+01:00"},
 "nodekey:b":{"HostName":"office","DNSName":"office-1.example-tailnet.ts.net.","CurAddr":"","Relay":"lhr","Online":true,"LastHandshake":"0001-01-01T00:00:00Z"}}}`

func TestParseTailscale(t *testing.T) {
	ts, err := parseTailscale([]byte(tsJSON), "garibaldi")
	if err != nil {
		t.Fatal(err)
	}
	if ts.Path != "direct" || ts.Endpoint != "203.0.113.10:51826" || ts.Relay != "lhr" || ts.Online == nil || !*ts.Online {
		t.Errorf("got %+v", ts)
	}
	if ts.LastHandshake == nil || ts.LastHandshake.Format(time.RFC3339) != "2026-10-13T20:36:00Z" {
		t.Errorf("handshake: %v", ts.LastHandshake)
	}
	rel, err := parseTailscale([]byte(tsJSON), "office-1") // matched on the DNS name
	if err != nil || rel.Path != "relay" || rel.Endpoint != "" || rel.LastHandshake != nil {
		t.Errorf("relayed peer: %+v, %v", rel, err)
	}
	if _, err := parseTailscale([]byte(tsJSON), "nope"); err == nil {
		t.Error("missing peer: want an error")
	}
	if _, err := parseTailscale([]byte("not json"), "garibaldi"); err == nil {
		t.Error("bad json: want an error")
	}
}

func TestMedian(t *testing.T) {
	if m, _ := median([]float64{3, 1, 2}); m != 2 {
		t.Errorf("odd: %v", m)
	}
	if m, _ := median([]float64{4, 1, 2, 3}); m != 2.5 {
		t.Errorf("even: %v", m)
	}
	if _, ok := median(nil); ok {
		t.Error("empty: want !ok")
	}
}

func fakePing(results map[string]PingStats) func(context.Context, string, int, time.Duration) PingStats {
	return func(_ context.Context, h string, _ int, _ time.Duration) PingStats { return results[h] }
}

func TestInternetDownOnlyIfAllFail(t *testing.T) {
	hosts := []string{"a", "b", "c"}
	ok := PingStats{Sent: 3, Received: 3, RTTs: []float64{10, 12, 14}}
	lossy := PingStats{Sent: 3, Received: 1, RTTs: []float64{30}}
	dead := PingStats{Sent: 3}
	r := internet(context.Background(), hosts, 3, time.Second, fakePing(map[string]PingStats{"a": dead, "b": ok, "c": lossy}))
	if !r.OK || r.TargetsOK != 2 || r.Targets != 3 {
		t.Fatalf("got %+v", r)
	}
	if *r.LossPct < 33.3 || *r.LossPct > 33.4 { // 2 lost of 6 sent by the hosts that answered
		t.Errorf("loss %v", *r.LossPct)
	}
	if *r.RTTms != 13 {
		t.Errorf("rtt %v", *r.RTTms)
	}
	r = internet(context.Background(), hosts, 3, time.Second, fakePing(map[string]PingStats{}))
	if r.OK || r.TargetsOK != 0 || r.LossPct != nil || r.Err == "" {
		t.Errorf("all dead: %+v", r)
	}
}

func TestPingResult(t *testing.T) {
	r := pingResult("gw", PingStats{Sent: 3, Received: 0, Err: ""})
	if r.OK || r.Err != "no reply" || *r.LossPct != 100 || r.RTTms != nil {
		t.Errorf("no reply: %+v", r)
	}
	r = pingResult("gw", PingStats{Err: "icmp socket: permission denied"})
	if r.OK || r.LossPct != nil || !strings.Contains(r.Err, "permission") {
		t.Errorf("no socket: %+v", r)
	}
}

func TestDNSFailsFast(t *testing.T) {
	// Nothing answers DNS on this port.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	start := time.Now()
	r := DNS(context.Background(), pc.LocalAddr().String(), "example.com", 300*time.Millisecond)
	if r.OK || r.Err == "" {
		t.Errorf("got %+v", r)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestPingLoopback(t *testing.T) {
	st := Ping(context.Background(), "127.0.0.1", 2, time.Second)
	if strings.HasPrefix(st.Err, "icmp socket") {
		t.Skipf("unprivileged ICMP not allowed here: %s", st.Err)
	}
	if st.Sent != 2 || st.Received != 2 || len(st.RTTs) != 2 {
		t.Errorf("got %+v", st)
	}
}

func TestRoundUsesHooks(t *testing.T) {
	s := &System{
		Cfg: Config{Internet: []string{"i1"}, TailscalePeer: "garibaldi", Count: 1, Timeout: 100 * time.Millisecond},
		Ping: fakePing(map[string]PingStats{
			"10.0.0.1": {Sent: 1, Received: 1, RTTs: []float64{1}},
			"i1":       {Sent: 1, Received: 1, RTTs: []float64{20}},
		}),
		GatewayFn:  func() (string, error) { return "10.0.0.1", nil },
		TailscaleF: func(context.Context, string) (*Tailscale, error) { return &Tailscale{Path: "direct"}, nil },
	}
	r := s.Round(context.Background())
	if !r.Gateway.OK || r.Gateway.Target != "10.0.0.1" || !r.Internet.OK || r.Bridge != nil || r.Tailscale == nil {
		t.Errorf("got %+v", r)
	}
}
