package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/ingest"
	"github.com/cauebraz/ehlo/internal/metrics"
	"github.com/cauebraz/ehlo/internal/provider"
	"github.com/cauebraz/ehlo/internal/warmup"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func newServerWith(t *testing.T, targets warmup.Targets) (*Server, *prometheus.Registry) {
	t.Helper()
	clock := func() time.Time { return now }
	reg := prometheus.NewRegistry()
	cls := provider.New(provider.Default)
	eng, err := warmup.New(warmup.DefaultPolicy(), targets, cls.Names(), warmup.WithClock(clock), warmup.WithKnownSources([]string{"mx-1"}))
	if err != nil {
		t.Fatal(err)
	}
	reg.MustRegister(metrics.NewWarmup(eng.Lanes))
	m := metrics.New(reg, "test", metrics.WithClock(clock))
	s := New(ingest.New(cls, m, eng), eng, reg, WithMaxBody(1<<16), WithClock(clock))
	return s, reg
}

func newServer(t *testing.T) (*Server, *prometheus.Registry) {
	return newServerWith(t, warmup.Targets{Default: 100_000, Warm: []string{"warm-1"}})
}

// get runs a GET and returns the recorder.
func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// value returns the sample of a series, or 0 when it does not exist.
func value(t *testing.T, reg *prometheus.Registry, name string, labels ...string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	next:
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if v, ok := want[l.GetName()]; ok && v != l.GetValue() {
					continue next
				}
			}
			switch {
			case m.Counter != nil:
				return m.GetCounter().GetValue()
			case m.Gauge != nil:
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEventsObjectArrayAndNDJSON(t *testing.T) {
	s, reg := newServer(t)
	h := s.Handler()
	one := `{"type":"attempt","time":"2026-03-10T11:00:00Z","source":"mx-1","domain":"gmail.com","count":3}`
	if rec := post(t, h, "/v1/events", one); rec.Code != http.StatusAccepted {
		t.Fatalf("object: %d %s", rec.Code, rec.Body)
	}
	arr := `[{"type":"reply","time":"2026-03-10T11:00:00Z","source":"mx-1","domain":"gmail.com","code":250,"enhanced":"2.0.0","count":2},
	         {"type":"reply","time":"2026-03-10T11:00:00Z","source":"mx-1","domain":"gmail.com","code":421,"enhanced":"4.7.28"}]`
	if rec := post(t, h, "/v1/events", arr); rec.Code != http.StatusAccepted {
		t.Fatalf("array: %d %s", rec.Code, rec.Body)
	}
	nd := "{\"type\":\"complaint\",\"time\":\"2026-03-10T11:00:00Z\",\"source\":\"mx-1\",\"domain\":\"gmail.com\"}\n{\"type\":\"open\",\"time\":\"2026-03-10T11:00:00Z\",\"source\":\"mx-1\",\"domain\":\"GMAIL.com\"}\n"
	if rec := post(t, h, "/v1/events", nd); rec.Code != http.StatusAccepted {
		t.Fatalf("ndjson: %d %s", rec.Code, rec.Body)
	}
	if got := value(t, reg, "ehlo_smtp_attempts_total", "source", "mx-1", "provider", "gmail", "attempt", "first"); got != 3 {
		t.Errorf("attempts = %v, want 3", got)
	}
	if got := value(t, reg, "ehlo_smtp_replies_total", "source", "mx-1", "provider", "gmail", "code", "421 4.7.28"); got != 1 {
		t.Errorf("421 replies = %v, want 1", got)
	}
	if got := value(t, reg, "ehlo_delivered_total", "source", "mx-1", "provider", "gmail"); got != 2 {
		t.Errorf("delivered = %v, want 2", got)
	}
	if got := value(t, reg, "ehlo_opens_total", "source", "mx-1", "provider", "gmail"); got != 1 {
		t.Errorf("opens with upper-case domain = %v, want 1", got)
	}
	d := s.engine.Decide("mx-1", "gmail")
	if d.Sent != 3 || d.Limit != 50 {
		t.Errorf("warmup lane = %+v, want sent 3 of 50", d)
	}
}

func TestProviderTagsSurviveIngest(t *testing.T) {
	s, reg := newServer(t)
	h := s.Handler()
	body := `[{"type":"reply","time":"2026-03-10T11:00:00Z","source":"mx-1","domain":"comcast.net","code":554,"text":"found on one or more DNSBLs, see http://postmaster.comcast.net/smtp-error-codes.php#BL000001"},
	         {"type":"reply","time":"2026-03-10T11:00:00Z","source":"mx-1","domain":"orange.fr","code":421,"text":"Too many connections, slow down. OFR004_104 [104]"},
	         {"type":"reply","time":"2026-03-10T11:00:00Z","source":"mx-1","domain":"t-online.de","code":554,"text":"IP=203.0.113.1 - A problem occurred. (BL)"}]`
	rec := post(t, h, "/v1/events", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("provider replies: %d %s", rec.Code, rec.Body)
	}
	var res IngestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Invalid != 0 {
		t.Errorf("replies with provider tags rejected: %+v", res)
	}
	if got := value(t, reg, "ehlo_reply_categories_total", "provider", "comcast", "category", "reputation_block"); got != 1 {
		t.Errorf("comcast BL000001 category = %v, want reputation_block 1", got)
	}
	if got := value(t, reg, "ehlo_smtp_replies_total", "provider", "orange", "code", "421 OFR004_104"); got != 1 {
		t.Errorf("orange code label with tag = %v, want 1", got)
	}
}

func TestInvalidEventsAreCountedAndReported(t *testing.T) {
	s, reg := newServer(t)
	h := s.Handler()
	body := `[{"type":"attempt","time":"2026-03-10T11:00:00Z","source":"mx-1"},{"type":"reply","time":"2026-03-10T11:00:00Z","source":"mx-1","code":99}]`
	rec := post(t, h, "/v1/events", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("mixed batch: %d %s", rec.Code, rec.Body)
	}
	var res IngestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 1 || res.Invalid != 1 || len(res.Errors) != 1 {
		t.Errorf("result = %+v", res)
	}
	if got := value(t, reg, "ehlo_events_total", "type", "reply", "result", "invalid"); got != 1 {
		t.Errorf("invalid reply events = %v, want 1", got)
	}
	if rec := post(t, h, "/v1/events", `[{"type":"nope","time":"2026-03-10T11:00:00Z","source":"x"}]`); rec.Code != http.StatusBadRequest {
		t.Errorf("all invalid: %d, want 400", rec.Code)
	}
	if rec := post(t, h, "/v1/events", `garbage`); rec.Code != http.StatusBadRequest {
		t.Errorf("garbage: %d, want 400", rec.Code)
	}
	if rec := post(t, h, "/v1/events", ``); rec.Code != http.StatusBadRequest {
		t.Errorf("empty: %d, want 400", rec.Code)
	}
	if rec := post(t, h, "/v1/events", `[`+strings.Repeat(`{"type":"open","time":"2026-03-10T11:00:00Z","source":"mx-1"},`, 2000)+`]`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d, want 413", rec.Code)
	}
}

func TestLimitsAPI(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	rec := get(t, h, "/v1/limits/mx-1/gmail")
	if rec.Code != http.StatusOK {
		t.Fatalf("limit: %d %s", rec.Code, rec.Body)
	}
	var d warmup.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if !d.Managed || d.Limit != 50 || d.Remaining != 50 || d.State != warmup.Raising || !d.EngagedOnly {
		t.Errorf("decision = %+v", d)
	}
	if d.DayEnd.Sub(d.DayStart) != 24*time.Hour {
		t.Errorf("day window = %v to %v", d.DayStart, d.DayEnd)
	}
	rec = get(t, h, "/v1/limits/warm-1/outlook")
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.State != warmup.AtTarget || d.EngagedOnly {
		t.Errorf("warm source = %+v", d)
	}
	rec = get(t, h, "/v1/limits")
	var all LimitsResult
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil || len(all.Lanes) != 2 {
		t.Errorf("lanes = %+v, %v, want 2", all.Lanes, err)
	}
}

func TestUnmanagedSourceIs404(t *testing.T) {
	s, _ := newServerWith(t, warmup.Targets{Default: 0})
	if rec := get(t, s.Handler(), "/v1/limits/x/gmail"); rec.Code != http.StatusNotFound {
		t.Errorf("unmanaged source: %d, want 404", rec.Code)
	}
}

func TestWebhooksAndMetrics(t *testing.T) {
	s, reg := newServer(t)
	h := s.Handler()
	ses := `{"eventType":"Bounce","bounce":{"bounceType":"Permanent","bouncedRecipients":[{"emailAddress":"user4@gmail.com"}]},"mail":{"timestamp":"2026-03-10T08:00:00Z"}}`
	if rec := post(t, h, "/v1/webhooks/ses", ses); rec.Code != http.StatusAccepted {
		t.Fatalf("ses: %d %s", rec.Code, rec.Body)
	}
	sg := `[{"email":"user5@outlook.com","timestamp":1773129600,"event":"delivered"}]`
	if rec := post(t, h, "/v1/webhooks/sendgrid", sg); rec.Code != http.StatusAccepted {
		t.Fatalf("sendgrid: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h, "/v1/webhooks/sendgrid", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("sendgrid bad body: %d, want 400", rec.Code)
	}
	if got := value(t, reg, "ehlo_bounces_total", "route", "ses", "provider", "gmail", "kind", "hard"); got != 1 {
		t.Errorf("ses hard bounces = %v, want 1", got)
	}
	if got := value(t, reg, "ehlo_delivered_total", "route", "sendgrid", "provider", "outlook"); got != 1 {
		t.Errorf("sendgrid delivered = %v, want 1", got)
	}
	s.engine.Observe(&event.Event{Type: event.Attempt, Time: now, Source: "mx-9", Provider: "gmail"}, warmup.SignalNone)
	body := get(t, h, "/metrics").Body.String()
	for _, want := range []string{
		`ehlo_bounces_total{kind="hard",provider="gmail",route="ses",source="ses"} 1`,
		`ehlo_warmup_state{provider="gmail",source="mx-9",state="raising"} 1`,
		`ehlo_build_info{version="test"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestDefaultClockAnswersInUTC(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("UTC-3", -3*60*60)
	t.Cleanup(func() { time.Local = saved })
	reg := prometheus.NewRegistry()
	cls := provider.New(provider.Default)
	eng, err := warmup.New(warmup.DefaultPolicy(), warmup.Targets{Default: 100_000}, cls.Names(), warmup.WithKnownSources([]string{"mx-1"}))
	if err != nil {
		t.Fatal(err)
	}
	s := New(ingest.New(cls, metrics.New(reg, "test"), eng), eng, reg)
	for _, path := range []string{"/v1/limits", "/v1/limits/mx-1/gmail"} {
		var body struct {
			ServerTime string `json:"server_time"`
		}
		if err := json.Unmarshal(get(t, s.Handler(), path).Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(body.ServerTime, "Z") {
			t.Errorf("%s server_time = %q, want UTC", path, body.ServerTime)
		}
	}
}
