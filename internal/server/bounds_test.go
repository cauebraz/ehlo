package server

import (
	"net/http"
	"net/http/httptest"
	"runtime"
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

func newServerWithDefaultMaxBody(t *testing.T) (*Server, *prometheus.Registry) {
	t.Helper()
	clock := func() time.Time { return now }
	reg := prometheus.NewRegistry()
	cls := provider.New(provider.Default)
	eng, err := warmup.New(warmup.DefaultPolicy(), warmup.Targets{}, cls.Names(), warmup.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New(reg, "test", metrics.WithClock(clock))
	return New(ingest.New(cls, m, eng), eng, reg, WithMaxBody(DefaultMaxBody), WithClock(clock)), reg
}

func TestLargeBatchAllocationIsBounded(t *testing.T) {
	const size = DefaultMaxBody
	const limit = 64 << 20
	s, _ := newServerWithDefaultMaxBody(t)
	h := s.Handler()
	for name, body := range map[string]string{
		"ndjson": strings.Repeat("{}\n", size/3),
		"array":  "[" + strings.Repeat("{},", size/3-1) + "{}]",
	} {
		t.Run(name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			rec := post(t, h, "/v1/events", body)
			runtime.ReadMemStats(&after)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %d, want 413 for more than %d events", rec.Code, event.MaxBatch)
			}
			got := after.TotalAlloc - before.TotalAlloc
			t.Logf("allocated %d KiB", got>>10)
			if got > limit {
				t.Errorf("allocated %d MiB for a %d MiB body, want at most %d MiB", got>>20, size>>20, limit>>20)
			}
		})
	}
}

func TestBatchOverTheEventLimitRecordsNothing(t *testing.T) {
	s, reg := newServerWithDefaultMaxBody(t)
	h := s.Handler()
	one := `{"type":"open","time":"2026-03-10T11:00:00Z","source":"mx-1","domain":"gmail.com"}`
	for name, body := range map[string]string{
		"ndjson": strings.Repeat(one+"\n", event.MaxBatch+1),
		"array":  "[" + strings.Repeat(one+",", event.MaxBatch) + one + "]",
	} {
		t.Run(name, func(t *testing.T) {
			rec := post(t, h, "/v1/events", body)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %d %s, want 413", rec.Code, rec.Body)
			}
		})
	}
	if got := value(t, reg, "ehlo_opens_total", "provider", "gmail"); got != 0 {
		t.Errorf("opens = %v, want 0: a refused batch records nothing", got)
	}
	rec := post(t, h, "/v1/events", strings.Repeat(one+"\n", event.MaxBatch))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d for exactly %d events, want 202", rec.Code, event.MaxBatch)
	}
	if got := value(t, reg, "ehlo_opens_total", "provider", "gmail"); got != event.MaxBatch {
		t.Errorf("opens = %v, want %d", got, event.MaxBatch)
	}
}

func TestMalformedEventRecordsNothing(t *testing.T) {
	s, reg := newServer(t)
	h := s.Handler()
	one := `{"type":"open","time":"2026-03-10T11:00:00Z","source":"mx-1","domain":"gmail.com"}`
	for name, body := range map[string]string{
		"ndjson": one + "\n" + one + "\n{\"type\":",
		"array":  "[" + one + "," + one + ",{]",
		"trail":  "[" + one + "] x",
	} {
		t.Run(name, func(t *testing.T) {
			if rec := post(t, h, "/v1/events", body); rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d %s, want 400", rec.Code, rec.Body)
			}
		})
	}
	if got := value(t, reg, "ehlo_opens_total", "provider", "gmail"); got != 0 {
		t.Errorf("opens = %v, want 0", got)
	}
}

func TestEmptyBatchIsRejected(t *testing.T) {
	s, _ := newServer(t)
	for _, body := range []string{"[]", " [ ] ", "\n\n"} {
		rec := post(t, s.Handler(), "/v1/events", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %q = %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
}

func TestSendGridBatchOverTheEventLimitIsRejected(t *testing.T) {
	s, _ := newServerWithDefaultMaxBody(t)
	one := `{"email":"user1@gmail.com","timestamp":1773129601,"event":"delivered"}`
	body := "[" + strings.Repeat(one+",", event.MaxBatch) + one + "]"
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/sendgrid", strings.NewReader(body))
	rec := httptest.NewRecorder()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	s.Handler().ServeHTTP(rec, req)
	runtime.ReadMemStats(&after)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	empty := "[" + strings.Repeat("{},", DefaultMaxBody/3-1) + "{}]"
	runtime.GC()
	runtime.ReadMemStats(&before)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/webhooks/sendgrid", strings.NewReader(empty)))
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 64<<20 {
		t.Errorf("allocated %d MiB for a 4 MiB SendGrid body, want at most 64 MiB", got>>20)
	}
}

func TestSESRecipientListOverTheEventLimitRecordsNothing(t *testing.T) {
	s, reg := newServerWithDefaultMaxBody(t)
	rcpts := `"` + strings.Repeat(`user1@gmail.com","`, event.MaxBatch) + `user1@gmail.com"`
	body := `{"eventType":"Delivery","delivery":{"recipients":[` + rcpts + `]},"mail":{}}`
	rec := post(t, s.Handler(), "/v1/webhooks/ses", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d %s, want 413 for more than %d recipients", rec.Code, rec.Body, event.MaxBatch)
	}
	if got := value(t, reg, "ehlo_delivered_total", "provider", "gmail"); got != 0 {
		t.Errorf("delivered = %v, want 0", got)
	}

	rcpt := `"a@gmail.com",`
	big := `{"eventType":"Delivery","delivery":{"recipients":[` + strings.Repeat(rcpt, (DefaultMaxBody-100)/len(rcpt)) + `"a@gmail.com"]},"mail":{}}`
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	rec = post(t, s.Handler(), "/v1/webhooks/ses", big)
	runtime.ReadMemStats(&after)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d for a 4 MiB recipient list, want 413", rec.Code)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 64<<20 {
		t.Errorf("allocated %d MiB for a 4 MiB SES body, want at most 64 MiB", got>>20)
	}
}

func TestWebhookOverTheEventLimitRecordsNothing(t *testing.T) {
	s, reg := newServer(t)
	one := event.Event{Type: event.Open, Time: now, Source: "mx-1", Domain: "gmail.com"}
	parse := func([]byte, time.Time) ([]event.Event, error) {
		events := make([]event.Event, event.MaxBatch+1)
		for i := range events {
			events[i] = one
		}
		return events, nil
	}
	pass := func(http.ResponseWriter, *http.Request, []byte) bool { return false }
	rec := httptest.NewRecorder()
	s.webhook(parse, pass)(rec, httptest.NewRequest(http.MethodPost, "/v1/webhooks/test", strings.NewReader("{}")))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 for %d events", rec.Code, event.MaxBatch+1)
	}
	if got := value(t, reg, "ehlo_opens_total", "provider", "gmail"); got != 0 {
		t.Errorf("opens = %v, want 0", got)
	}
}
