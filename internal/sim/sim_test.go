package sim

import (
	"context"
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/classify"
	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/ingest"
	"github.com/cauebraz/ehlo/internal/provider"
	"github.com/cauebraz/ehlo/internal/warmup"
)

var start = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

// engineLimits runs the warmup engine in process, the way the server would,
// and feeds every event back into it.
type engineLimits struct {
	e   *warmup.Engine
	cls *provider.Classifier
	now func() time.Time
}

func (l *engineLimits) Decide(_ context.Context, source, prov string) (warmup.Decision, error) {
	return l.e.Decide(source, prov), nil
}

func (l *engineLimits) feed(events []event.Event) {
	for i := range events {
		e := &events[i]
		if e.Provider == "" {
			e.Provider = l.cls.Classify(e.Domain)
		}
		l.e.Observe(e, ingest.Signal(classify.Event(e)))
	}
}

func newSim(t *testing.T, sc Scenario, targets warmup.Targets) (*Sim, *engineLimits) {
	t.Helper()
	cls := provider.New(provider.Default)
	var s *Sim
	eng, err := warmup.New(warmup.DefaultPolicy(), targets, cls.Names(), warmup.WithClock(func() time.Time { return s.Clock() }))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Scenario: sc, Sources: []string{"mx-1", "mx-2", "mx-3", "mx-4"}, DailyVolume: 600_000,
		Baselines: []string{"ses", "sendgrid"}, BaselineShare: 0.1,
		QueueRunDelay: 5 * time.Minute, QueueLifetime: 24 * time.Hour, Start: start, Seed: 7,
	}
	if sc == Warmup {
		cfg.NewSource = "mx-new"
	}
	l := &engineLimits{e: eng, cls: cls}
	s = New(cfg, l)
	l.now = s.Clock
	return s, l
}

// run steps the simulator through d in steps of step and returns the totals
// per key and every event emitted, in order.
func run(t *testing.T, s *Sim, l *engineLimits, d, step time.Duration) (map[string]float64, []event.Event) {
	t.Helper()
	totals := map[string]float64{}
	var all []event.Event
	for elapsed := time.Duration(0); elapsed < d; elapsed += step {
		events, errs := s.Step(context.Background(), step)
		if len(errs) > 0 {
			t.Fatalf("errors: %v", errs)
		}
		for i := range events {
			if err := events[i].Validate(); err != nil {
				t.Fatalf("invalid event %+v: %v", events[i], err)
			}
			totals[key(&events[i])] += float64(events[i].N())
		}
		all = append(all, events...)
		l.feed(events)
	}
	return totals, all
}

func key(e *event.Event) string {
	k := string(e.Type) + "/" + e.Source
	switch e.Type {
	case event.Attempt:
		kind := e.Attempt
		if kind == "" {
			kind = event.First
		}
		k += "/" + string(kind)
	case event.Reply:
		k += "/" + string(e.Class()) + "/" + e.CodeLabel()
	case event.Bounce:
		k += "/" + string(e.Bounce)
	case event.DMARC:
		k += "/" + string(e.SPF) + "/" + string(e.DKIM)
	}
	return k
}

var warm = warmup.Targets{Default: 200_000, Warm: []string{"mx-1", "mx-2", "mx-3", "mx-4"}, Mix: map[string]float64{"gmail": 0.55, "outlook": 0.2, "yahoo": 0.1}}

func TestSteadyDayRespectsLimitsAndSplits(t *testing.T) {
	t.Parallel()
	s, l := newSim(t, Steady, warm)
	totals, _ := run(t, s, l, 24*time.Hour, 5*time.Minute)
	var self, base float64
	for _, src := range []string{"mx-1", "mx-2", "mx-3", "mx-4"} {
		self += totals["attempt/"+src+"/first"]
	}
	base = totals["attempt/ses/first"] + totals["attempt/sendgrid/first"]
	if ratio := base / (self + base); ratio < 0.08 || ratio > 0.12 {
		t.Errorf("baseline share = %.3f, want about 0.10", ratio)
	}
	if self < 500_000 || self > 600_000 {
		t.Errorf("self-hosted attempts in a day = %.0f, want 90%% of 600k", self)
	}
	for _, d := range l.e.Lanes() {
		if d.Sent > d.Limit+1 {
			t.Errorf("%s/%s sent %.0f above its limit %.0f", d.Source, d.Provider, d.Sent, d.Limit)
		}
	}
	if totals["reply/mx-1/connect/connect"] <= 0 {
		t.Error("no connection failures reported on a steady day")
	}
	if totals["dmarc/mx-1/pass/fail"] != 0 {
		t.Errorf("unsigned mail on a steady day = %.0f", totals["dmarc/mx-1/pass/fail"])
	}
}

func TestWarmupScenarioAddsANewSource(t *testing.T) {
	t.Parallel()
	s, l := newSim(t, Warmup, warm)
	_, events := run(t, s, l, 12*time.Hour, time.Minute)
	d, _ := l.Decide(context.Background(), "mx-new", "gmail")
	if d.State != warmup.Raising || d.Sent > d.Limit+1 || d.Sent == 0 {
		t.Errorf("new source lane = %+v, want raising, sent within the limit of 50", d)
	}
	steps, engaged := 0, 0
	for _, e := range events {
		if e.Type == event.Queue {
			steps++
		}
		if steps == 0 || e.Source != "mx-new" || e.Type != event.Attempt || e.Attempt == event.Retry {
			continue
		}
		if e.Engaged == nil || !*e.Engaged {
			t.Errorf("new source attempt after the server saw it without engaged=true: %+v", e)
			break
		}
		engaged++
	}
	if engaged == 0 {
		t.Error("no engaged attempts from the new source after the first step")
	}
}

func TestOverCapacityFillsEverySource(t *testing.T) {
	t.Parallel()
	s, l := newSim(t, OverCapacity, warm)
	run(t, s, l, 23*time.Hour, 5*time.Minute)
	saturated := 0
	for _, d := range l.e.Lanes() {
		if d.Provider == "gmail" && d.Sent >= 0.8*d.Limit {
			saturated++
		}
	}
	if saturated != 4 {
		t.Errorf("gmail lanes near their limit = %d, want 4 at 2x the normal volume (the hourly cap trims the peaks)", saturated)
	}
}

func TestGmailThrottleOnTwoSources(t *testing.T) {
	t.Parallel()
	s, l := newSim(t, GmailThrottle, warm)
	totals, _ := run(t, s, l, 6*time.Hour, time.Minute)
	for _, src := range []string{"mx-1", "mx-2", "mx-3", "mx-4"} {
		limited := totals["reply/"+src+"/4xx/421 4.7.28"]
		attempts := totals["attempt/"+src+"/first"]
		ratio := limited / attempts
		d, _ := l.Decide(context.Background(), src, "gmail")
		throttled := src == "mx-3" || src == "mx-4"
		if throttled && (ratio < 0.02 || d.Backoffs == 0) {
			t.Errorf("%s 421 ratio = %.3f backoffs = %d, want rate limiting and at least one backoff", src, ratio, d.Backoffs)
		}
		if !throttled && (ratio > 0.01 || d.Backoffs != 0) {
			t.Errorf("%s 421 ratio = %.3f backoffs = %d, want neither", src, ratio, d.Backoffs)
		}
	}
}

func TestConnectRefused(t *testing.T) {
	t.Parallel()
	s, l := newSim(t, ConnectRefused, warm)
	totals, _ := run(t, s, l, 2*time.Hour, time.Minute)
	if r := totals["reply/mx-1/connect/connect"] / totals["attempt/mx-1/first"]; r < 0.08 {
		t.Errorf("connect failure ratio = %.3f, want >= 0.08 with outlook at 60%% and 20%% of the mix", r)
	}
}

func TestComplaintSpikePausesTheLane(t *testing.T) {
	t.Parallel()
	s, l := newSim(t, ComplaintSpike, warm)
	run(t, s, l, 24*time.Hour, 5*time.Minute)
	d, _ := l.Decide(context.Background(), "mx-4", "gmail")
	if d.State != warmup.Paused {
		t.Errorf("mx-4 gmail after a day at 0.4%% complaints = %s, want paused", d.State)
	}
	if d, _ := l.Decide(context.Background(), "mx-1", "gmail"); d.State != warmup.AtTarget {
		t.Errorf("mx-1 gmail = %s, want full", d.State)
	}
}

func TestDKIMDownOnOneSource(t *testing.T) {
	t.Parallel()
	s, l := newSim(t, DKIMDown, warm)
	totals, _ := run(t, s, l, 25*time.Hour, 5*time.Minute)
	if totals["dmarc/mx-3/pass/fail"] <= 0 || totals["dmarc/mx-1/pass/fail"] != 0 {
		t.Errorf("unsigned dmarc rows: mx-3 %.0f mx-1 %.0f, want only mx-3", totals["dmarc/mx-3/pass/fail"], totals["dmarc/mx-1/pass/fail"])
	}
}

func TestUnknownSourceReportsDailyFailures(t *testing.T) {
	t.Parallel()
	s, l := newSim(t, UnknownSource, warm)
	totals, _ := run(t, s, l, 25*time.Hour, 5*time.Minute)
	if totals["dmarc/unknown/fail/fail"] != unknownSourceDaily {
		t.Errorf("unknown source rows = %.0f, want %d", totals["dmarc/unknown/fail/fail"], unknownSourceDaily)
	}
}

func TestQueueRetriesAndExpiry(t *testing.T) {
	t.Parallel()
	s, l := newSim(t, Steady, warm)
	totals, events := run(t, s, l, 30*time.Hour, 5*time.Minute)
	if totals["attempt/mx-1/first"] == 0 {
		t.Fatal("no attempts")
	}
	if totals["bounce/mx-1/soft"] <= 0 {
		t.Error("no soft bounces after the queue lifetime")
	}
	last := events[len(events)-1]
	if last.Type != event.Queue {
		t.Errorf("last event of a step = %s, want queue snapshots", last.Type)
	}
}

func TestUnmanagedServerStillSends(t *testing.T) {
	t.Parallel()
	s, l := newSim(t, Steady, warmup.Targets{Default: 0})
	totals, _ := run(t, s, l, time.Hour, time.Minute)
	if totals["attempt/mx-1/first"] <= 0 {
		t.Error("no attempts when the server manages no source")
	}
}
