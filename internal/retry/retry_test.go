package retry

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

var startup = Backoff{Initial: time.Second, Max: 60 * time.Second}

// failingConnect fails n times with err, then succeeds, counting its calls.
func failingConnect(n int, err error, calls *int) func() error {
	return func() error {
		*calls++
		if *calls <= n {
			return err
		}
		return nil
	}
}

var errNoHost = errors.New("dial tcp: lookup broker: no such host")

// recordWaits is a Wait that records each backoff instead of sleeping.
func recordWaits(waits *[]time.Duration) Wait {
	return func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return ctx.Err()
	}
}

// captureLog redirects the standard logger for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	return &buf
}

func TestConnect_BacksOffToCapThenSucceeds(t *testing.T) {
	var calls int
	var waits []time.Duration
	res, err := Connect(context.Background(), "MQTT", startup, failingConnect(8, errNoHost, &calls), recordWaits(&waits))
	if err != nil {
		t.Fatalf("Connect() = %v, want nil once the broker answers", err)
	}
	if calls != 9 || res.Attempts != 9 {
		t.Errorf("connect called %d times, Attempts %d; want 9 (8 failures + 1 success)", calls, res.Attempts)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v seconds", waits, want)
	}
	var total time.Duration
	for i, w := range want {
		if waits[i] != w*time.Second {
			t.Errorf("wait %d = %s, want %s", i, waits[i], w*time.Second)
		}
		total += w * time.Second
	}
	if res.Waited != total {
		t.Errorf("Waited = %s, want %s", res.Waited, total)
	}
}

func TestConnect_CapIsConfigurable(t *testing.T) {
	var calls int
	var waits []time.Duration
	b := Backoff{Initial: time.Second, Max: 30 * time.Second}
	if _, err := Connect(context.Background(), "MQTT", b, failingConnect(7, errNoHost, &calls), recordWaits(&waits)); err != nil {
		t.Fatal(err)
	}
	if got := waits[len(waits)-1]; got != 30*time.Second {
		t.Errorf("last wait = %s, want the 30 s cap", got)
	}
}

func TestConnect_FirstAttemptSucceeds_NoWait(t *testing.T) {
	var calls int
	var waits []time.Duration
	res, err := Connect(context.Background(), "MQTT", startup, failingConnect(0, errNoHost, &calls), recordWaits(&waits))
	if err != nil {
		t.Fatalf("Connect() = %v", err)
	}
	if calls != 1 || len(waits) != 0 || res.Attempts != 1 || res.Waited != 0 {
		t.Errorf("calls = %d, waits = %v, res = %+v; want one call and no wait", calls, waits, res)
	}
}

// A SIGTERM while waiting between attempts must end the wait at once and
// return the context's error — a clean exit, not a fatal one, not a 60 s
// hang, and not one more attempt.
func TestConnect_SignalDuringBackoffExitsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	connect := func() error {
		calls++
		return errors.New("connection refused")
	}
	// The signal arrives once the backoff has begun.
	wait := func(ctx context.Context, d time.Duration) error {
		cancel()
		return Sleep(ctx, d)
	}

	start := time.Now()
	_, err := Connect(ctx, "MQTT", startup, connect, wait)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Connect() = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("connect called %d times, want 1 — no attempt after the signal", calls)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("took %s to stop, want the 1 s backoff cut short", elapsed)
	}
}

// A SIGTERM during an attempt (connect returns once ctx is cancelled) must
// exit without logging a failure or starting a backoff.
func TestConnect_SignalDuringAttemptExitsCleanly(t *testing.T) {
	buf := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	var calls, waits int
	connect := func() error {
		calls++
		cancel()
		return ctx.Err()
	}
	wait := func(context.Context, time.Duration) error {
		waits++
		return nil
	}

	_, err := Connect(ctx, "MQTT", startup, connect, wait)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Connect() = %v, want context.Canceled", err)
	}
	if calls != 1 || waits != 0 {
		t.Errorf("calls = %d, waits = %d; want 1 attempt and no backoff", calls, waits)
	}
	if buf.Len() != 0 {
		t.Errorf("logged %q, want nothing for an attempt cut short by a signal", buf.String())
	}
}

// Hours of identical failures — the 6½-hour DNS break in
// docs/live-tests.md — must not produce a line every 30 s, but must stay
// visible: the first failure, then one summary line per RepeatLogEvery.
func TestConnect_RepeatedFailureLoggedSparingly(t *testing.T) {
	buf := captureLog(t)
	var calls int
	var waits []time.Duration
	b := Backoff{Initial: time.Second, Max: 30 * time.Second}
	// 30 minutes at the 30 s cap is about 60 attempts.
	if _, err := Connect(context.Background(), "MQTT", b, failingConnect(65, errNoHost, &calls), recordWaits(&waits)); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 || len(lines) > 8 {
		t.Fatalf("logged %d lines for 65 identical failures over ~30 min, want a handful:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "MQTT connection failed: "+errNoHost.Error()+" — retrying in 1s") {
		t.Errorf("first line = %q, want the first failure with its error", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, "still failing after") || !strings.Contains(l, "no such host") {
			t.Errorf("repeat line = %q, want a summary carrying the error", l)
		}
	}
}

// A different error is news and is logged at once: it says the fault moved,
// e.g. from DNS to a refused connection.
func TestConnect_ChangedErrorLoggedImmediately(t *testing.T) {
	buf := captureLog(t)
	errs := []error{errNoHost, errNoHost, errNoHost, errors.New("connection refused"), nil}
	i := 0
	connect := func() error { e := errs[i]; i++; return e }
	var waits []time.Duration
	if _, err := Connect(context.Background(), "MQTT", startup, connect, recordWaits(&waits)); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if n := strings.Count(out, "no such host"); n != 1 {
		t.Errorf("the repeated DNS error was logged %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("the changed error was not logged:\n%s", out)
	}
	if !strings.Contains(out, "(2 not logged)") {
		t.Errorf("the line after suppressed attempts should say how many were skipped:\n%s", out)
	}
}

func TestSleep(t *testing.T) {
	if err := Sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("Sleep() = %v, want nil after the wait", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := Sleep(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep() = %v, want context.Canceled", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Error("a cancelled Sleep waited")
	}
}
