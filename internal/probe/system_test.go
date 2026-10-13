package probe

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDefaultGateway(t *testing.T) {
	old := RouteFile
	t.Cleanup(func() { RouteFile = old })
	RouteFile = filepath.Join(t.TempDir(), "route")
	if _, err := DefaultGateway(); err == nil || !strings.Contains(err.Error(), "no routing table") {
		t.Errorf("missing file: %v", err)
	}
	if err := os.WriteFile(RouteFile, []byte(piRoute), 0o644); err != nil {
		t.Fatal(err)
	}
	if gw, err := DefaultGateway(); err != nil || gw != "192.168.0.1" {
		t.Errorf("got %q, %v", gw, err)
	}
}

func TestTailscaleStatus(t *testing.T) {
	old := TailscaleCommand
	t.Cleanup(func() { TailscaleCommand = old })
	TailscaleCommand = func(ctx context.Context) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("tailscale run without a deadline")
		}
		return []byte(tsJSON), nil
	}
	if ts, err := TailscaleStatus(context.Background(), "garibaldi"); err != nil || ts.Path != "direct" {
		t.Errorf("got %+v, %v", ts, err)
	}
	TailscaleCommand = func(context.Context) ([]byte, error) { return nil, errors.New("exit status 1") }
	if _, err := TailscaleStatus(context.Background(), "garibaldi"); err == nil || !strings.Contains(err.Error(), "tailscale status") {
		t.Errorf("failing command: %v", err)
	}
}

func TestResolve4(t *testing.T) {
	if ip, err := resolve4(context.Background(), "192.0.2.1"); err != nil || ip.String() != "192.0.2.1" {
		t.Errorf("literal: %v %v", ip, err)
	}
	if _, err := resolve4(context.Background(), "2001:db8::1"); err == nil || !strings.Contains(err.Error(), "not an IPv4") {
		t.Errorf("IPv6 literal: %v", err)
	}
	if _, err := resolve4(context.Background(), "no-such-host.invalid"); err == nil {
		t.Error("unresolvable name accepted")
	}
	if ip, err := resolve4(context.Background(), "localhost"); err != nil || !ip.IsLoopback() {
		t.Errorf("localhost: %v %v", ip, err)
	}
}

// dnsServer answers every A query on a local UDP port with answer, or with
// no records when answer is nil.
func dnsServer(t *testing.T, answer net.IP) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true})
			b.EnableCompression()
			b.StartQuestions() //nolint:errcheck
			b.Question(q)      //nolint:errcheck
			b.StartAnswers()   //nolint:errcheck
			if answer != nil && q.Type == dnsmessage.TypeA {
				var a [4]byte
				copy(a[:], answer.To4())
				b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: a}) //nolint:errcheck
			}
			out, _ := b.Finish()
			pc.WriteTo(out, addr) //nolint:errcheck
		}
	}()
	return pc.LocalAddr().String()
}

func TestDNSAnswers(t *testing.T) {
	r := DNS(context.Background(), dnsServer(t, net.ParseIP("192.0.2.7")), "example.com", time.Second)
	if !r.OK || r.RTTms == nil || r.Err != "" {
		t.Errorf("got %+v", r)
	}
}

func TestDNSNoAddresses(t *testing.T) {
	r := DNS(context.Background(), dnsServer(t, nil), "example.com", time.Second)
	if r.OK || r.Err == "" {
		t.Errorf("an empty answer passed: %+v", r)
	}
}

func TestDNSNotConfigured(t *testing.T) {
	if r := DNS(context.Background(), "", "example.com", time.Second); r.OK || !strings.Contains(r.Err, "no DNS server") {
		t.Errorf("got %+v", r)
	}
}
