package ingest_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cauebraz/ehlo/internal/classify"
	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/ingest"
	"github.com/cauebraz/ehlo/internal/metrics"
	"github.com/cauebraz/ehlo/internal/provider"
	"github.com/cauebraz/ehlo/internal/warmup"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

// newPipeline wires a pipeline over a fresh registry and a default-policy
// engine that manages mx-1 toward every default provider.
func newPipeline(t *testing.T) (*ingest.Pipeline, *prometheus.Registry, *warmup.Engine) {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	cls := provider.New(provider.Default)
	m := metrics.New(reg, "test", metrics.WithClock(func() time.Time { return now }))
	eng, err := warmup.New(warmup.DefaultPolicy(), warmup.Targets{Default: 10000}, cls.Names(), warmup.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	return ingest.New(cls, m, eng), reg, eng
}

func series(t *testing.T, reg prometheus.Gatherer, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	next:
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if want, ok := labels[l.GetName()]; ok && want != l.GetValue() {
					continue next
				}
			}
			return m.GetCounter().GetValue(), true
		}
	}
	return 0, false
}

func reply(code int, enhanced, text string) *event.Event {
	return &event.Event{Type: event.Reply, Time: now, Source: "mx-1", Domain: "yahoo.com", Code: code, Enhanced: enhanced, Stage: event.StageRcpt, Text: text}
}

func TestIngestEnrichesAndThrottles(t *testing.T) {
	p, reg, eng := newPipeline(t)
	if err := p.Ingest(&event.Event{Type: event.Attempt, Time: now, Source: "mx-1", Domain: "yahoo.com", Count: 20}); err != nil {
		t.Fatal(err)
	}
	if d := eng.Decide("mx-1", "yahoo"); d.State != warmup.Raising {
		t.Fatalf("before throttling: state %s, want raising", d.State)
	}

	e := reply(421, "4.7.0", "421 4.7.0 [TSS04] Messages from 203.0.113.1 temporarily deferred due to unexpected volume or user complaints")
	e.Count = 5
	if err := p.Ingest(e); err != nil {
		t.Fatal(err)
	}
	if e.Provider != "yahoo" || e.Tag != "TSS04" {
		t.Errorf("enriched provider=%q tag=%q, want yahoo TSS04", e.Provider, e.Tag)
	}
	if got, ok := series(t, reg, "ehlo_reply_categories_total", map[string]string{"provider": "yahoo", "category": "rate_limited"}); !ok || got != 5 {
		t.Errorf("rate_limited for yahoo = %v (%v), want 5", got, ok)
	}
	if got, ok := series(t, reg, "ehlo_smtp_replies_total", map[string]string{"code": "421 4.7.0 TSS04"}); !ok || got != 5 {
		t.Errorf("reply code series = %v (%v), want 5 with the tag in the label", got, ok)
	}
	d := eng.Decide("mx-1", "yahoo")
	if d.State != warmup.BackedOff || d.AllowedNow != 0 || d.ResumeAt == nil {
		t.Errorf("after 5 of 20 attempts throttled: %+v, want backoff with allowed_now 0", d)
	}
	if !d.ResumeAt.Equal(now.Add(10 * time.Minute)) {
		t.Errorf("resume_at = %v, want %v", d.ResumeAt, now.Add(10*time.Minute))
	}
}

func TestIngestClassifiesGmailRateText(t *testing.T) {
	p, reg, _ := newPipeline(t)
	e := reply(421, "4.7.28", "421-4.7.28 Our system has detected an unusual rate of unsolicited mail")
	e.Domain = "googlemail.com"
	if err := p.Ingest(e); err != nil {
		t.Fatal(err)
	}
	if e.Provider != "gmail" || e.Tag != "" {
		t.Errorf("provider=%q tag=%q, want gmail with no tag", e.Provider, e.Tag)
	}
	if got, ok := series(t, reg, "ehlo_reply_categories_total", map[string]string{"provider": "gmail", "category": "rate_limited"}); !ok || got != 1 {
		t.Errorf("rate_limited for gmail = %v (%v), want 1", got, ok)
	}
}

func TestIngestKeepsExplicitProviderAndTag(t *testing.T) {
	p, _, _ := newPipeline(t)
	e := reply(421, "4.7.0", "421 4.7.0 [TSS04] slow down")
	e.Provider, e.Tag = "outlook", "KEEP"
	if err := p.Ingest(e); err != nil {
		t.Fatal(err)
	}
	if e.Provider != "outlook" || e.Tag != "KEEP" {
		t.Errorf("provider=%q tag=%q, want the caller's values kept", e.Provider, e.Tag)
	}
}

func TestIngestInvalidIsCountedNotObserved(t *testing.T) {
	p, reg, eng := newPipeline(t)
	e := reply(250, "2.0.0", "250 OK")
	e.Source = ""
	err := p.Ingest(e)
	if !errors.Is(err, event.ErrInvalid) || !strings.Contains(err.Error(), "source") {
		t.Fatalf("error = %v, want ErrInvalid about source", err)
	}
	if got, ok := series(t, reg, "ehlo_events_total", map[string]string{"type": "reply", "result": "invalid"}); !ok || got != 1 {
		t.Errorf("invalid replies = %v (%v), want 1", got, ok)
	}
	if _, ok := series(t, reg, "ehlo_events_total", map[string]string{"type": "reply", "result": "accepted"}); ok {
		t.Error("an accepted series exists for the invalid reply")
	}
	if _, ok := series(t, reg, "ehlo_reply_categories_total", nil); ok {
		t.Error("the invalid reply reached the category series")
	}
	if _, ok := series(t, reg, "ehlo_delivered_total", nil); ok {
		t.Error("the invalid reply reached the delivered series")
	}
	if n := len(eng.Lanes()); n != 0 {
		t.Errorf("lanes = %d, want none for an invalid event", n)
	}
}

func TestSignal(t *testing.T) {
	want := map[classify.Category]warmup.Signal{
		classify.RateLimited:     warmup.SignalThrottled,
		classify.ReputationBlock: warmup.SignalBlocked,
	}
	for _, cat := range classify.Categories {
		exp := warmup.SignalNone
		if s, ok := want[cat]; ok {
			exp = s
		}
		if got := ingest.Signal(cat); got != exp {
			t.Errorf("Signal(%s) = %d, want %d", cat, got, exp)
		}
	}
	if got := ingest.Signal(""); got != warmup.SignalNone {
		t.Errorf("Signal(\"\") = %d, want none", got)
	}
}

func TestIngestMapsUnknownProviderToOther(t *testing.T) {
	p, _, _ := newPipeline(t)
	e := reply(250, "2.0.0", "")
	e.Provider = "made-up-provider"
	if err := p.Ingest(e); err != nil {
		t.Fatal(err)
	}
	if e.Provider != provider.Other {
		t.Errorf("provider = %q, want %q", e.Provider, provider.Other)
	}
}

func TestIngestRejectsUnconfiguredRoute(t *testing.T) {
	p, _, _ := newPipeline(t)
	e := reply(250, "2.0.0", "")
	e.Route = "zz-probe"
	if err := p.Ingest(e); !errors.Is(err, event.ErrInvalid) {
		t.Errorf("unconfigured route err = %v, want ErrInvalid", err)
	}
	e.Route = "ses"
	if err := p.Ingest(e); err != nil {
		t.Errorf("builtin route err = %v", err)
	}
}

func TestIngestBoundsSources(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	cls := provider.New(provider.Default)
	m := metrics.New(reg, "test", metrics.WithClock(func() time.Time { return now }))
	eng, err := warmup.New(warmup.DefaultPolicy(), warmup.Targets{Default: 10000}, cls.Names(), warmup.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	p := ingest.New(cls, m, eng, ingest.WithSources(2, []string{"mx-1"}))
	for _, src := range []string{"mx-1", "mx-2", "mx-1"} {
		e := reply(250, "2.0.0", "")
		e.Source = src
		if err := p.Ingest(e); err != nil {
			t.Fatalf("source %s: %v", src, err)
		}
	}
	e := reply(250, "2.0.0", "")
	e.Source = "mx-3"
	if err := p.Ingest(e); !errors.Is(err, ingest.ErrTooManySources) {
		t.Errorf("third source err = %v, want ErrTooManySources", err)
	}
	if n := len(eng.Lanes()); n != 2 {
		t.Errorf("lanes = %d, want 2", n)
	}
}
