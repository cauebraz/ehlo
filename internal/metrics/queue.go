package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// QueueStaleAfter is how long a source's queue gauges outlive its last
// queue event. After it, ehlo_queue_deferred_messages and
// ehlo_queue_oldest_age_seconds for the source disappear until the next
// queue event, so a stopped queue reporter cannot keep an alert firing on
// its last value.
const QueueStaleAfter = 15 * time.Minute

type queueSample struct {
	deferred, oldest float64
	at               time.Time
}

// queueGauges exports the last queue event per source at scrape time and
// drops sources whose last event is older than QueueStaleAfter.
type queueGauges struct {
	now              func() time.Time
	deferred, oldest *prometheus.Desc

	mu      sync.Mutex
	sources map[string]queueSample
}

func newQueueGauges(now func() time.Time) *queueGauges {
	return &queueGauges{
		now:      now,
		deferred: prometheus.NewDesc("ehlo_queue_deferred_messages", "Deferred queue size from the last queue event.", []string{"source"}, nil),
		oldest:   prometheus.NewDesc("ehlo_queue_oldest_age_seconds", "Oldest deferred message from the last queue event.", []string{"source"}, nil),
		sources:  map[string]queueSample{},
	}
}

func (q *queueGauges) set(source string, deferred, oldest float64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sources[source] = queueSample{deferred: deferred, oldest: oldest, at: q.now()}
}

// Describe sends both descriptors.
func (q *queueGauges) Describe(ch chan<- *prometheus.Desc) {
	ch <- q.deferred
	ch <- q.oldest
}

// Collect emits the fresh sources and forgets the stale ones.
func (q *queueGauges) Collect(ch chan<- prometheus.Metric) {
	now := q.now()
	q.mu.Lock()
	defer q.mu.Unlock()
	for src, s := range q.sources {
		if now.Sub(s.at) > QueueStaleAfter {
			delete(q.sources, src)
			continue
		}
		ch <- prometheus.MustNewConstMetric(q.deferred, prometheus.GaugeValue, s.deferred, src)
		ch <- prometheus.MustNewConstMetric(q.oldest, prometheus.GaugeValue, s.oldest, src)
	}
}
