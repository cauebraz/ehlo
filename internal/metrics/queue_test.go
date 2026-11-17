package metrics_test

import (
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/metrics"
)

func TestQueueGaugesExpireWithoutQueueEvents(t *testing.T) {
	clock := now
	reg := prometheus.NewPedanticRegistry()
	m := metrics.New(reg, "test", metrics.WithClock(func() time.Time { return clock }))
	q := func(src string, deferred int) *event.Event {
		e := base(event.Queue)
		e.Source, e.Deferred, e.OldestAge = src, deferred, time.Minute
		return e
	}
	m.Observe(q("mx-1", 5000), "")
	m.Observe(q("mx-2", 10), "")
	deferred := func(src string) float64 {
		return value(t, reg, "ehlo_queue_deferred_messages", map[string]string{"source": src})
	}
	if got := deferred("mx-1"); got != 5000 {
		t.Fatalf("mx-1 deferred = %v, want 5000", got)
	}

	clock = clock.Add(metrics.QueueStaleAfter - time.Second)
	m.Observe(q("mx-2", 20), "")
	if got := deferred("mx-1"); got != 5000 {
		t.Errorf("mx-1 deferred just inside the window = %v, want 5000", got)
	}

	clock = clock.Add(2 * time.Second)
	if got := deferred("mx-1"); !math.IsNaN(got) {
		t.Errorf("mx-1 deferred = %v %s after the last queue event, want the series gone", got, metrics.QueueStaleAfter)
	}
	if got := value(t, reg, "ehlo_queue_oldest_age_seconds", map[string]string{"source": "mx-1"}); !math.IsNaN(got) {
		t.Errorf("mx-1 oldest age = %v after it went stale, want the series gone", got)
	}
	if got := deferred("mx-2"); got != 20 {
		t.Errorf("mx-2 deferred = %v, want 20 from its fresh event", got)
	}

	m.Observe(q("mx-1", 7), "")
	if got := deferred("mx-1"); got != 7 {
		t.Errorf("mx-1 deferred after a new queue event = %v, want 7", got)
	}
}
