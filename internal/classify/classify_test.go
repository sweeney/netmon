package classify

import (
	"testing"

	"github.com/sweeney/netmon/internal/probe"
)

func f(v float64) *float64 { return &v }

func up() probe.Result { return probe.Result{OK: true, RTTms: f(10), LossPct: f(0)} }

func round(gw, inet, dns, broker bool) probe.Round {
	r := probe.Round{Gateway: up(), Internet: up(), DNS: up(), Broker: up()}
	r.Gateway.OK, r.Internet.OK, r.DNS.OK, r.Broker.OK = gw, inet, dns, broker
	return r
}

func TestRawOrder(t *testing.T) {
	th := Thresholds{LossPct: 20, RTTms: 500}
	cases := []struct {
		name string
		r    probe.Round
		want Status
	}{
		{"all up", round(true, true, true, true), OK},
		{"gateway down explains everything", round(false, false, false, false), LANDown},
		{"gateway down alone", round(false, true, true, true), LANDown},
		{"internet down", round(true, false, false, false), WANDown},
		{"broker down", round(true, true, true, false), TailnetDown},
		{"dns down only", round(true, true, false, true), Degraded},
	}
	for _, c := range cases {
		if got := Raw(c.r, th); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestRawDegraded(t *testing.T) {
	th := Thresholds{LossPct: 20, RTTms: 500}
	r := round(true, true, true, true)
	r.Internet.LossPct = f(33.3)
	if got := Raw(r, th); got != Degraded {
		t.Errorf("loss: got %s", got)
	}
	r = round(true, true, true, true)
	r.Broker.RTTms = f(800)
	if got := Raw(r, th); got != Degraded {
		t.Errorf("rtt: got %s", got)
	}
	r = round(true, true, true, true)
	r.Internet.LossPct = f(20) // at the threshold, not over it
	if got := Raw(r, th); got != OK {
		t.Errorf("at threshold: got %s", got)
	}
	if got := Raw(r, Thresholds{}); got != OK {
		t.Errorf("no thresholds: got %s", got)
	}
}

func TestIsOutage(t *testing.T) {
	for s, want := range map[Status]bool{OK: false, Degraded: false, Unknown: false, TailnetDown: true, WANDown: true, LANDown: true} {
		if s.IsOutage() != want {
			t.Errorf("%q.IsOutage() = %v", s, !want)
		}
	}
}

func TestHysteresis(t *testing.T) {
	type step struct {
		raw     Status
		want    Status
		changed bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"first ok needs up_after", []step{{OK, Unknown, false}, {OK, OK, true}, {OK, OK, false}}},
		{"first outage needs down_after", []step{{WANDown, Unknown, false}, {WANDown, Unknown, false}, {WANDown, WANDown, true}}},
		{"one dropped round is not an outage", []step{{OK, Unknown, false}, {OK, OK, true}, {LANDown, OK, false}, {OK, OK, false}, {LANDown, OK, false}, {LANDown, OK, false}, {OK, OK, false}}},
		{"three bad rounds are", []step{{OK, Unknown, false}, {OK, OK, true}, {WANDown, OK, false}, {WANDown, OK, false}, {WANDown, WANDown, true}}},
		{"recovery needs up_after", []step{{OK, Unknown, false}, {OK, OK, true}, {LANDown, OK, false}, {LANDown, OK, false}, {LANDown, LANDown, true}, {OK, LANDown, false}, {OK, OK, true}}},
		{"escalation needs down_after", []step{
			{WANDown, Unknown, false}, {WANDown, Unknown, false}, {WANDown, WANDown, true},
			{LANDown, WANDown, false}, {LANDown, WANDown, false}, {LANDown, LANDown, true}}},
		{"easing within an outage needs up_after", []step{
			{LANDown, Unknown, false}, {LANDown, Unknown, false}, {LANDown, LANDown, true},
			{TailnetDown, LANDown, false}, {TailnetDown, TailnetDown, true}}},
		{"a run interrupted by the current status starts again", []step{
			{OK, Unknown, false}, {OK, OK, true},
			{WANDown, OK, false}, {WANDown, OK, false}, {OK, OK, false}, {WANDown, OK, false}, {WANDown, OK, false}, {WANDown, WANDown, true}}},
		{"flapping between outage kinds settles on the worst", []step{
			{OK, Unknown, false}, {OK, OK, true},
			{LANDown, OK, false}, {WANDown, OK, false}, {LANDown, LANDown, true},
			{WANDown, LANDown, false}, {LANDown, LANDown, false}, {WANDown, LANDown, false}}},
		{"recovery with latency spikes still ends the outage", []step{
			{WANDown, Unknown, false}, {WANDown, Unknown, false}, {WANDown, WANDown, true},
			{OK, WANDown, false}, {Degraded, Degraded, true}, {OK, Degraded, false}, {Degraded, Degraded, false}, {OK, Degraded, false}, {OK, OK, true}}},
		{"first status: mixed non-outage rounds", []step{{Degraded, Unknown, false}, {OK, Degraded, true}}},
		{"direction change starts a new run", []step{
			{WANDown, Unknown, false}, {WANDown, Unknown, false}, {WANDown, WANDown, true},
			{LANDown, WANDown, false}, {LANDown, WANDown, false}, {OK, WANDown, false}, {OK, OK, true}}},
		{"degraded is worse than ok", []step{{OK, Unknown, false}, {OK, OK, true}, {Degraded, OK, false}, {Degraded, OK, false}, {Degraded, Degraded, true}}},
	}
	for _, c := range cases {
		h := Hysteresis{DownAfter: 3, UpAfter: 2}
		for i, s := range c.steps {
			got, changed := h.Observe(s.raw)
			if got != s.want || changed != s.changed {
				t.Errorf("%s: step %d (%s): got %s/%v, want %s/%v", c.name, i, s.raw, got, changed, s.want, s.changed)
				break
			}
		}
	}
}

func TestObserveAtDatesFromTheRunsFirstRound(t *testing.T) {
	h := Hysteresis{DownAfter: 3, UpAfter: 2}
	h.ObserveAt(OK, 0)
	h.ObserveAt(OK, 10)
	h.ObserveAt(WANDown, 20)
	h.ObserveAt(OK, 30) // interrupts the run
	h.ObserveAt(WANDown, 40)
	h.ObserveAt(WANDown, 50)
	st, changed, since := h.ObserveAt(WANDown, 60)
	if st != WANDown || !changed || since != 40 {
		t.Errorf("got %s %v %v, want wan_down true 40", st, changed, since)
	}
}
