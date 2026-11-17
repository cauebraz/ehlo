package metrics_test

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cauebraz/ehlo/internal/classify"
	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/metrics"
	"github.com/cauebraz/ehlo/internal/warmup"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

type sample struct {
	name   string
	labels map[string]string
	value  float64
}

func ptr(b bool) *bool { return &b }

func lane(labels ...string) map[string]string {
	m := map[string]string{"route": "self_hosted", "source": "mx-1", "provider": "gmail"}
	for i := 0; i+1 < len(labels); i += 2 {
		m[labels[i]] = labels[i+1]
	}
	return m
}

func base(t event.Type) *event.Event {
	return &event.Event{Type: t, Time: now.Add(-30 * time.Second), Source: "mx-1", Provider: "gmail"}
}

// metric is one gathered series: value for a counter or gauge, count and
// sum for a histogram.
type metric struct {
	value float64
	count uint64
	sum   float64
}

// find returns the series in the gathered families whose name and labels
// match.
func find(t *testing.T, reg prometheus.Gatherer, name string, labels map[string]string) (metric, bool) {
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
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if len(got) != len(labels) {
				continue
			}
			for k, v := range labels {
				if got[k] != v {
					continue next
				}
			}
			out := metric{value: math.NaN()}
			switch {
			case m.GetCounter() != nil:
				out.value = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				out.value = m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				out.count = m.GetHistogram().GetSampleCount()
				out.sum = m.GetHistogram().GetSampleSum()
				out.value = float64(out.count)
			}
			return out, true
		}
	}
	return metric{}, false
}

// count returns how many series the registry gathers, all of them or only
// those with one of the names.
func count(t *testing.T, reg prometheus.Gatherer, names ...string) int {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range families {
		if len(names) > 0 && !slices.Contains(names, f.GetName()) {
			continue
		}
		n += len(f.GetMetric())
	}
	return n
}

// value returns the series value, or NaN when it is missing.
func value(t *testing.T, reg prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	m, ok := find(t, reg, name, labels)
	if !ok {
		return math.NaN()
	}
	return m.value
}

func TestObserve(t *testing.T) {
	attemptRetry := base(event.Attempt)
	attemptRetry.Attempt, attemptRetry.Signed, attemptRetry.Engaged, attemptRetry.Count = event.Retry, ptr(true), ptr(true), 3
	attemptNo := base(event.Attempt)
	attemptNo.Signed, attemptNo.Engaged, attemptNo.Route = ptr(false), ptr(false), "esp"
	delivered := base(event.Reply)
	delivered.Code, delivered.Enhanced, delivered.Stage, delivered.Delay = 250, "2.0.0", event.StageData, 1.5
	deliveredNoDelay := base(event.Reply)
	deliveredNoDelay.Code, deliveredNoDelay.Enhanced = 250, "2.0.0"
	connect := base(event.Reply)
	connect.Stage = event.StageConnect
	throttled := base(event.Reply)
	throttled.Code, throttled.Enhanced, throttled.Tag, throttled.Stage = 421, "4.7.0", "TSS04", event.StageMail
	bounce := func(k event.BounceKind) *event.Event {
		e := base(event.Bounce)
		e.Bounce = k
		return e
	}
	queue := base(event.Queue)
	queue.Deferred, queue.OldestAge = 12, 90*time.Second
	dmarc := base(event.DMARC)
	dmarc.SPF, dmarc.DKIM = event.Pass, event.Fail
	tls := base(event.TLS)
	tls.TLSVersion, tls.TLSTrust = "TLSv1.3", "verified"

	cases := []struct {
		name string
		ev   *event.Event
		cat  classify.Category
		want []sample
	}{
		{"attempt first unknown flags", base(event.Attempt), "", []sample{
			{"ehlo_smtp_attempts_total", lane("attempt", "first"), 1},
			{"ehlo_dkim_messages_total", map[string]string{"route": "self_hosted", "source": "mx-1", "result": "unknown"}, 1},
			{"ehlo_audience_messages_total", lane("audience", "unknown"), 1},
		}},
		{"attempt retry signed engaged count", attemptRetry, "", []sample{
			{"ehlo_smtp_attempts_total", lane("attempt", "retry"), 3},
			{"ehlo_dkim_messages_total", map[string]string{"route": "self_hosted", "source": "mx-1", "result": "signed"}, 3},
			{"ehlo_audience_messages_total", lane("audience", "engaged"), 3},
		}},
		{"attempt unsigned all on another route", attemptNo, "", []sample{
			{"ehlo_smtp_attempts_total", lane("route", "esp", "attempt", "first"), 1},
			{"ehlo_dkim_messages_total", map[string]string{"route": "esp", "source": "mx-1", "result": "unsigned"}, 1},
			{"ehlo_audience_messages_total", lane("route", "esp", "audience", "all"), 1},
		}},
		{"reply 2xx with delay", delivered, classify.Delivered, []sample{
			{"ehlo_smtp_replies_total", lane("class", "2xx", "code", "250 2.0.0"), 1},
			{"ehlo_reply_categories_total", lane("category", "delivered"), 1},
			{"ehlo_delivered_total", lane(), 1},
			{"ehlo_delivery_seconds", map[string]string{"route": "self_hosted", "provider": "gmail"}, 1},
		}},
		{"reply 2xx without delay", deliveredNoDelay, classify.Delivered, []sample{
			{"ehlo_delivered_total", lane(), 1},
			{"ehlo_delivery_seconds", map[string]string{"route": "self_hosted", "provider": "gmail"}, 0},
		}},
		{"reply connect level", connect, classify.Connection, []sample{
			{"ehlo_smtp_replies_total", lane("class", "connect", "code", "connect"), 1},
			{"ehlo_smtp_connect_failures_total", lane(), 1},
			{"ehlo_reply_categories_total", lane("category", "connection"), 1},
		}},
		{"reply 4xx with tag and category", throttled, classify.RateLimited, []sample{
			{"ehlo_smtp_replies_total", lane("class", "4xx", "code", "421 4.7.0 TSS04"), 1},
			{"ehlo_reply_categories_total", lane("category", "rate_limited"), 1},
		}},
		{"bounce hard", bounce(event.Hard), "", []sample{{"ehlo_bounces_total", lane("kind", "hard"), 1}}},
		{"bounce soft", bounce(event.Soft), "", []sample{{"ehlo_bounces_total", lane("kind", "soft"), 1}}},
		{"bounce policy", bounce(event.Policy), "", []sample{{"ehlo_bounces_total", lane("kind", "policy"), 1}}},
		{"complaint", base(event.Complaint), "", []sample{{"ehlo_complaints_total", lane(), 1}}},
		{"open", base(event.Open), "", []sample{{"ehlo_opens_total", lane(), 1}}},
		{"queue gauges", queue, "", []sample{
			{"ehlo_queue_deferred_messages", map[string]string{"source": "mx-1"}, 12},
			{"ehlo_queue_oldest_age_seconds", map[string]string{"source": "mx-1"}, 90},
		}},
		{"dmarc", dmarc, "", []sample{
			{"ehlo_dmarc_messages_total", map[string]string{"source": "mx-1", "spf": "pass", "dkim": "fail"}, 1},
		}},
		{"tls", tls, "", []sample{
			{"ehlo_smtp_tls_sessions_total", map[string]string{"route": "self_hosted", "source": "mx-1", "version": "TLSv1.3", "trust": "verified"}, 1},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewPedanticRegistry()
			m := metrics.New(reg, "test", metrics.WithClock(func() time.Time { return now }))
			m.Observe(tc.ev, tc.cat)
			for _, w := range tc.want {
				got, ok := find(t, reg, w.name, w.labels)
				if !ok {
					if w.value == 0 {
						continue
					}
					t.Errorf("%s%v missing", w.name, w.labels)
					continue
				}
				if got.value != w.value {
					t.Errorf("%s%v = %v, want %v", w.name, w.labels, got.value, w.value)
				}
			}
			r := tc.ev.Route
			if r == "" {
				r = event.RouteSelfHosted
			}
			if last := value(t, reg, "ehlo_route_last_event_timestamp_seconds", map[string]string{"route": r, "type": string(tc.ev.Type)}); last != float64(now.Unix()) {
				t.Errorf("last event = %v, want %d", last, now.Unix())
			}
			lag, ok := find(t, reg, "ehlo_event_lag_seconds", map[string]string{"type": string(tc.ev.Type)})
			if !ok || lag.count != 1 || lag.sum != 30 {
				t.Errorf("lag = %+v, want one sample of 30s", lag)
			}
		})
	}
}

func TestCount(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := metrics.New(reg, "test")
	m.Count(event.Reply, true)
	m.Count(event.Reply, true)
	m.Count(event.Attempt, false)
	if got := value(t, reg, "ehlo_events_total", map[string]string{"type": "reply", "result": "accepted"}); got != 2 {
		t.Errorf("accepted replies = %v, want 2", got)
	}
	if got := value(t, reg, "ehlo_events_total", map[string]string{"type": "attempt", "result": "invalid"}); got != 1 {
		t.Errorf("invalid attempts = %v, want 1", got)
	}
}

func TestPrimeCreatesZeroSeries(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := metrics.New(reg, "test")
	if before := count(t, reg); before != 1 {
		t.Fatalf("series before prime = %d, want only build info", before)
	}
	m.Prime(event.RouteSelfHosted, "mx-1", []string{"gmail", "yahoo"})
	after := count(t, reg)
	tlsSeries := len(event.TLSVersions()) * len(event.TLSTrusts())
	perProvider := 2 + 1 + 1 + 1 + 1 + 1 + 3 + 2 + len(classify.Categories)
	want := 1 + tlsSeries + 2 + 2*perProvider + 4 + 2*len(event.Types)
	if after != want {
		t.Errorf("series after prime = %d, want %d", after, want)
	}
	for _, w := range []sample{
		{"ehlo_smtp_attempts_total", lane("provider", "yahoo", "attempt", "retry"), 0},
		{"ehlo_reply_categories_total", lane("category", "other"), 0},
		{"ehlo_bounces_total", lane("kind", "policy"), 0},
		{"ehlo_dmarc_messages_total", map[string]string{"source": "mx-1", "spf": "fail", "dkim": "fail"}, 0},
		{"ehlo_delivery_seconds", map[string]string{"route": "self_hosted", "provider": "gmail"}, 0},
		{"ehlo_events_total", map[string]string{"type": "tls", "result": "invalid"}, 0},
	} {
		got, ok := find(t, reg, w.name, w.labels)
		if !ok {
			t.Errorf("%s%v not primed", w.name, w.labels)
			continue
		}
		if got.value != 0 {
			t.Errorf("%s%v = %v, want 0", w.name, w.labels, got.value)
		}
	}
	if n := count(t, reg, "ehlo_smtp_tls_sessions_total"); n != tlsSeries {
		t.Errorf("tls series = %d, want %d", n, tlsSeries)
	}
}

func TestWarmupCollector(t *testing.T) {
	lanes := func() []warmup.Decision {
		return []warmup.Decision{
			{Source: "mx-1", Provider: "gmail", Limit: 300, State: warmup.Raising, Day: 3},
			{Source: "mx-2", Provider: "gmail", Limit: 100, State: warmup.Holding, Day: 1},
		}
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(metrics.NewWarmup(lanes))
	shares := 0.0
	for _, src := range []string{"mx-1", "mx-2"} {
		m, ok := find(t, reg, "ehlo_warmup_share_ratio", map[string]string{"source": src, "provider": "gmail"})
		if !ok {
			t.Fatalf("share for %s missing", src)
		}
		shares += m.value
	}
	if math.Abs(shares-1) > 1e-9 {
		t.Errorf("shares sum to %v, want 1", shares)
	}
	if got := value(t, reg, "ehlo_warmup_share_ratio", map[string]string{"source": "mx-1", "provider": "gmail"}); got != 0.75 {
		t.Errorf("mx-1 share = %v, want 0.75", got)
	}
	for src, want := range map[string]warmup.State{"mx-1": warmup.Raising, "mx-2": warmup.Holding} {
		ones := 0
		for _, st := range warmup.States() {
			m, ok := find(t, reg, "ehlo_warmup_state", map[string]string{"source": src, "provider": "gmail", "state": string(st)})
			if !ok {
				t.Fatalf("state %s for %s missing", st, src)
			}
			if m.value == 1 {
				ones++
				if st != want {
					t.Errorf("%s state 1 on %s, want %s", src, st, want)
				}
			}
		}
		if ones != 1 {
			t.Errorf("%s has %d state series at 1, want 1", src, ones)
		}
	}
	if n := count(t, reg, "ehlo_warmup_state"); n != 2*len(warmup.States()) {
		t.Errorf("state series = %d, want %d", n, 2*len(warmup.States()))
	}
	if got := value(t, reg, "ehlo_warmup_daily_limit_messages", map[string]string{"source": "mx-2", "provider": "gmail"}); got != 100 {
		t.Errorf("mx-2 limit = %v, want 100", got)
	}
	empty := prometheus.NewPedanticRegistry()
	empty.MustRegister(metrics.NewWarmup(func() []warmup.Decision { return nil }))
	if n := count(t, empty); n != 0 {
		t.Errorf("empty lanes emit %d series, want 0", n)
	}
}
