// Package retry repeats a connection attempt with capped exponential backoff
// until it succeeds or its context is cancelled.
//
// It serves both the startup MQTT connect and the reconnect after a lost
// connection.  The second can run for hours (a live test of ups-mqtt once had
// the broker's name unresolvable for 6½), so repeated failures
// are logged sparingly: the first, any whose error differs from the last,
// and otherwise one every RepeatLogEvery.
// Copied from github.com/sweeney/ups-mqtt internal/retry.
package retry

import (
	"context"
	"log"
	"time"
)

// Backoff is the wait between attempts: Initial, doubling, capped at Max.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
}

// RepeatLogEvery is how often an unchanged failure is logged again, so a long
// outage stays visible without a line every few seconds.
const RepeatLogEvery = 5 * time.Minute

// Wait sleeps for d, or returns ctx's error if it is cancelled first.
type Wait func(ctx context.Context, d time.Duration) error

// Result describes how a successful Connect got there.
type Result struct {
	Attempts int           // including the successful one
	Waited   time.Duration // total backoff between attempts
}

// Connect calls connect until it succeeds, waiting between attempts with
// backoff b.  wait ends early when ctx is cancelled, and its error — ctx's —
// is returned, so a signal during backoff exits cleanly.  An attempt that
// fails because ctx was cancelled mid-attempt is not logged as a failure.
func Connect(ctx context.Context, what string, b Backoff, connect func() error, wait Wait) (Result, error) {
	var (
		res        Result
		lastErr    string
		sinceLog   time.Duration
		suppressed int
	)
	backoff := b.Initial
	for {
		res.Attempts++
		err := connect()
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return res, ctx.Err() // the attempt was cut short by a signal
		}

		if msg := err.Error(); msg != lastErr || sinceLog >= RepeatLogEvery {
			switch {
			case res.Attempts == 1:
				log.Printf("%s connection failed: %v — retrying in %s", what, err, backoff)
			case suppressed > 0:
				log.Printf("%s connection still failing after %d attempts over %s (%d not logged): %v — retrying every %s",
					what, res.Attempts, res.Waited, suppressed, err, backoff)
			default:
				log.Printf("%s connection failed (attempt %d): %v — retrying in %s", what, res.Attempts, err, backoff)
			}
			lastErr, sinceLog, suppressed = msg, 0, 0
		} else {
			suppressed++
		}

		if err := wait(ctx, backoff); err != nil {
			return res, err
		}
		res.Waited += backoff
		sinceLog += backoff

		backoff *= 2
		if backoff > b.Max {
			backoff = b.Max
		}
	}
}

// Sleep is the production Wait: it waits for d, or returns ctx's error if it
// is cancelled first.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
