// Tests for real.go — in package publisher (not publisher_test) so that
// unexported helpers like newTLSConfig are accessible.
package publisher

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"
)

// makeTempCACert writes a self-signed CA certificate to a temp file and
// returns its path (caller is responsible for cleanup).
func makeTempCACert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"Test CA"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}
	f, err := os.CreateTemp("", "test-ca-*.pem")
	if err != nil {
		t.Fatalf("creating temp cert file: %v", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("encoding PEM: %v", err)
	}
	f.Close() //nolint:errcheck
	return f.Name()
}

// ── newTLSConfig ─────────────────────────────────────────────────────────────

func TestNewTLSConfig_NonexistentFile(t *testing.T) {
	_, err := newTLSConfig("/nonexistent/ca.pem")
	if err == nil {
		t.Fatal("expected error for non-existent CA cert file")
	}
}

func TestNewTLSConfig_InvalidPEM(t *testing.T) {
	f, err := os.CreateTemp("", "bad-ca-*.pem")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	defer os.Remove(f.Name())
	f.WriteString("this is not a valid PEM certificate") //nolint:errcheck
	f.Close()                                            //nolint:errcheck

	_, err = newTLSConfig(f.Name())
	if err == nil {
		t.Fatal("expected error for file with no valid PEM blocks")
	}
}

func TestNewTLSConfig_ValidCert(t *testing.T) {
	path := makeTempCACert(t)
	defer os.Remove(path)

	cfg, err := newTLSConfig(path)
	if err != nil {
		t.Fatalf("newTLSConfig: %v", err)
	}
	if cfg == nil || cfg.RootCAs == nil {
		t.Error("expected non-nil tls.Config with RootCAs set")
	}
}

// ── NewMQTTPublisher ─────────────────────────────────────────────────────────

// TestNewMQTTPublisher_TLSCertError verifies the error path when the TLS CA
// cert file cannot be loaded.  It surfaces from construction, not from
// Connect: no retry can fix a bad path, so it must not be retried.
func TestNewMQTTPublisher_TLSCertError(t *testing.T) {
	cfg := Options{
		Broker:    "tcp://127.0.0.1:1883",
		ClientID:  "test",
		TLSCACert: "/nonexistent/ca.pem",
	}
	_, err := NewMQTTPublisher(cfg, "ups/state", "{}")
	if err == nil {
		t.Fatal("expected error when TLS CA cert file does not exist")
	}
}

// TestNewMQTTPublisher_WithCredentials_TLSError verifies that username/password
// are applied and a subsequent TLS error is returned cleanly.
func TestNewMQTTPublisher_WithCredentials_TLSError(t *testing.T) {
	cfg := Options{
		Broker:    "tcp://127.0.0.1:1883",
		ClientID:  "test",
		Username:  "user",
		Password:  "pass",
		TLSCACert: "/nonexistent/ca.pem",
	}
	_, err := NewMQTTPublisher(cfg, "ups/state", "{}")
	if err == nil {
		t.Fatal("expected TLS error even with credentials set")
	}
}

// NewMQTTPublisher must not dial: an unreachable broker is the caller's to
// retry, not a construction error.
func TestNewMQTTPublisher_DoesNotConnect(t *testing.T) {
	cfg := Options{Broker: "tcp://127.0.0.1:1", ClientID: "test"}
	start := time.Now()
	p, err := NewMQTTPublisher(cfg, "ups/x/state", "{}")
	if err != nil {
		t.Fatalf("NewMQTTPublisher() = %v, want nil for an unreachable broker", err)
	}
	if p.client.IsConnected() {
		t.Error("client is connected before Connect was called")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s — construction must not wait on the network", elapsed)
	}
}

// The LWT is the broker announcing this client dead: an event, so it goes at
// the event QoS the caller passes, even though the state itself is QoS 0.
func TestNewMQTTPublisher_WillUsesEventQOS(t *testing.T) {
	cfg := Options{Broker: "tcp://127.0.0.1:1", ClientID: "test", WillQOS: 1}
	p, err := NewMQTTPublisher(cfg, "ups/x/state", `{"online":false}`)
	if err != nil {
		t.Fatalf("NewMQTTPublisher() = %v", err)
	}
	opts := p.client.OptionsReader()
	if !opts.WillEnabled() || opts.WillTopic() != "ups/x/state" || !opts.WillRetained() {
		t.Errorf("will = enabled %v topic %q retained %v, want a retained will on ups/x/state",
			opts.WillEnabled(), opts.WillTopic(), opts.WillRetained())
	}
	if got := opts.WillQos(); got != 1 {
		t.Errorf("will QoS = %d, want event QoS 1", got)
	}
}
