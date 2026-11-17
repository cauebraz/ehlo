// Package metrics turns delivery events into Prometheus series.
// docs/metrics.md lists them.
package metrics

import (
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cauebraz/ehlo/internal/classify"
	"github.com/cauebraz/ehlo/internal/event"
)

// Label values the series use for booleans the event may leave unset.
const (
	labelSigned   = "signed"
	labelUnsigned = "unsigned"
	labelEngaged  = "engaged"
	labelAll      = "all"
	labelUnknown  = "unknown"
	resultOK      = "accepted"
	resultInvalid = "invalid"
)

// Bounds on the code label of ehlo_smtp_replies_total. A lane is one
// route, source and provider. A lane keeps at most MaxCodesPerLane codes
// with an enhanced status or tag, and all lanes together keep at most
// MaxCodeSeries series beyond the fixed ones: the classify.KnownCodes, the
// connection stages and the class fallbacks. A reply past a bound counts
// under its bare three-digit code, or under its class (2xx, 4xx, 5xx) when
// the bare code would pass MaxCodeSeries.
const (
	MaxCodesPerLane = 64
	MaxCodeSeries   = 4096
)

var knownCodes = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range classify.KnownCodes {
		m[c] = true
	}
	return m
}()

type laneKey struct{ route, source, provider string }

// codeSet tracks the code labels in use, per lane and in total.
type codeSet struct {
	mu       sync.Mutex
	lanes    map[laneKey]map[string]bool
	detailed map[laneKey]int
	total    int
}

// Metrics holds every event-derived series.
type Metrics struct {
	events          *prometheus.CounterVec
	attempts        *prometheus.CounterVec
	replies         *prometheus.CounterVec
	categories      *prometheus.CounterVec
	connectFailures *prometheus.CounterVec
	delivered       *prometheus.CounterVec
	delivery        *prometheus.HistogramVec
	bounces         *prometheus.CounterVec
	complaints      *prometheus.CounterVec
	opens           *prometheus.CounterVec
	dkim            *prometheus.CounterVec
	audience        *prometheus.CounterVec
	tls             *prometheus.CounterVec
	dmarc           *prometheus.CounterVec
	queue           *queueGauges
	lastEvent       *prometheus.GaugeVec
	lag             *prometheus.HistogramVec
	snapshotAt      prometheus.Gauge
	snapshotFails   prometheus.Counter
	now             func() time.Time

	codes codeSet
}

// Option configures Metrics.
type Option func(*Metrics)

// WithClock replaces the wall clock used for receipt times.
func WithClock(now func() time.Time) Option {
	return func(m *Metrics) { m.now = now }
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
}

func gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

// New creates the series and registers them, with a build info gauge.
func New(reg prometheus.Registerer, version string, opts ...Option) *Metrics {
	lane := []string{"route", "source", "provider"}
	with := func(extra ...string) []string { return append(append([]string(nil), lane...), extra...) }
	m := &Metrics{
		events:          counter("ehlo_events_total", "Events received, by type and result (accepted, invalid).", "type", "result"),
		attempts:        counter("ehlo_smtp_attempts_total", "Delivery attempts, first or retry.", with("attempt")...),
		replies:         counter("ehlo_smtp_replies_total", "SMTP replies by class and code.", with("class", "code")...),
		categories:      counter("ehlo_reply_categories_total", "SMTP replies by category.", with("category")...),
		connectFailures: counter("ehlo_smtp_connect_failures_total", "Sessions that ended before a transaction.", lane...),
		delivered:       counter("ehlo_delivered_total", "Messages the provider accepted.", lane...),
		delivery: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ehlo_delivery_seconds",
			Help:    "Seconds from queue entry to the provider's 2xx, one sample per reply event.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 300, 900, 3600, 14400, 86400},
		}, []string{"route", "provider"}),
		bounces:    counter("ehlo_bounces_total", "Bounces by kind (hard, soft, policy).", with("kind")...),
		complaints: counter("ehlo_complaints_total", "Spam complaints.", lane...),
		opens:      counter("ehlo_opens_total", "Opens.", lane...),
		dkim:       counter("ehlo_dkim_messages_total", "Attempts by DKIM result (signed, unsigned, unknown).", "route", "source", "result"),
		audience:   counter("ehlo_audience_messages_total", "Attempts by audience (engaged, all, unknown).", with("audience")...),
		tls:        counter("ehlo_smtp_tls_sessions_total", "TLS sessions by version and trust.", "route", "source", "version", "trust"),
		dmarc:      counter("ehlo_dmarc_messages_total", "DMARC report rows by SPF and DKIM result.", "source", "spf", "dkim"),
		lastEvent:  gauge("ehlo_route_last_event_timestamp_seconds", "Time the last event per route and type was received.", "route", "type"),
		lag: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ehlo_event_lag_seconds",
			Help:    "Seconds between an event's own time and its receipt, by type. The log tail lag.",
			Buckets: []float64{1, 5, 15, 60, 300, 900, 3600, 14400},
		}, []string{"type"}),
		snapshotAt: prometheus.NewGauge(prometheus.GaugeOpts{Name: "ehlo_warmup_snapshot_last_success_timestamp_seconds",
			Help: "Time the warmup state was last saved to the state file, 0 before the first save."}),
		snapshotFails: prometheus.NewCounter(prometheus.CounterOpts{Name: "ehlo_warmup_snapshot_failures_total",
			Help: "Failed saves of the warmup state file."}),
		now:   time.Now,
		codes: codeSet{lanes: map[laneKey]map[string]bool{}, detailed: map[laneKey]int{}},
	}
	for _, o := range opts {
		o(m)
	}
	m.queue = newQueueGauges(m.now)
	build := gauge("ehlo_build_info", "Build information.", "version")
	build.WithLabelValues(version).Set(1)
	reg.MustRegister(m.events, m.attempts, m.replies, m.categories, m.connectFailures, m.delivered, m.delivery,
		m.bounces, m.complaints, m.opens, m.dkim, m.audience, m.tls, m.dmarc, m.queue, m.lastEvent, m.lag, m.snapshotAt, m.snapshotFails, build)
	return m
}

// Snapshot records the result of one save of the warmup state.
func (m *Metrics) Snapshot(err error) {
	if err != nil {
		m.snapshotFails.Inc()
		return
	}
	m.snapshotAt.Set(float64(m.now().Unix()))
}

func route(e *event.Event) string {
	if e.Route == "" {
		return event.RouteSelfHosted
	}
	return e.Route
}

func flag(p *bool, yes, no string) string {
	switch {
	case p == nil:
		return labelUnknown
	case *p:
		return yes
	}
	return no
}

// Count records whether an event was accepted. A type outside event.Types
// is counted as unknown, so a client cannot add label values.
func (m *Metrics) Count(t event.Type, accepted bool) {
	result := resultOK
	if !accepted {
		result = resultInvalid
	}
	typ := string(t)
	if !slices.Contains(event.Types, t) {
		typ = labelUnknown
	}
	m.events.WithLabelValues(typ, result).Inc()
}

// Observe records one valid event whose provider is set. cat is the
// category of a reply event and ignored for other types.
func (m *Metrics) Observe(e *event.Event, cat classify.Category) {
	n := float64(e.N())
	r := route(e)
	switch e.Type {
	case event.Attempt:
		kind := e.Attempt
		if kind == "" {
			kind = event.First
		}
		m.attempts.WithLabelValues(r, e.Source, e.Provider, string(kind)).Add(n)
		m.dkim.WithLabelValues(r, e.Source, flag(e.Signed, labelSigned, labelUnsigned)).Add(n)
		m.audience.WithLabelValues(r, e.Source, e.Provider, flag(e.Engaged, labelEngaged, labelAll)).Add(n)
	case event.Reply:
		m.replies.WithLabelValues(r, e.Source, e.Provider, string(e.Class()), m.codes.label(laneKey{r, e.Source, e.Provider}, e)).Add(n)
		m.categories.WithLabelValues(r, e.Source, e.Provider, string(cat)).Add(n)
		switch e.Class() {
		case event.ClassConnect:
			m.connectFailures.WithLabelValues(r, e.Source, e.Provider).Add(n)
		case event.Class2xx:
			m.delivered.WithLabelValues(r, e.Source, e.Provider).Add(n)
			if e.Delay > 0 {
				m.delivery.WithLabelValues(r, e.Provider).Observe(e.Delay)
			}
		}
	case event.Bounce:
		m.bounces.WithLabelValues(r, e.Source, e.Provider, string(e.Bounce)).Add(n)
	case event.Complaint:
		m.complaints.WithLabelValues(r, e.Source, e.Provider).Add(n)
	case event.Open:
		m.opens.WithLabelValues(r, e.Source, e.Provider).Add(n)
	case event.Queue:
		m.queue.set(e.Source, float64(e.Deferred), e.OldestAge.Seconds())
	case event.DMARC:
		m.dmarc.WithLabelValues(e.Source, string(e.SPF), string(e.DKIM)).Add(n)
	case event.TLS:
		m.tls.WithLabelValues(r, e.Source, e.TLSVersion, e.TLSTrust).Add(n)
	}
	now := m.now()
	m.lastEvent.WithLabelValues(r, string(e.Type)).Set(float64(now.Unix()))
	m.lag.WithLabelValues(string(e.Type)).Observe(max(0, now.Sub(e.Time).Seconds()))
}

// Prime creates a source's series at zero on a route, so rate() reads 0
// before the first event and a missing series means a stopped adapter.
func (m *Metrics) Prime(route, source string, providers []string) {
	r := route
	for _, v := range event.TLSVersions() {
		for _, trust := range event.TLSTrusts() {
			m.tls.WithLabelValues(r, source, v, trust)
		}
	}
	for _, res := range []string{labelSigned, labelUnsigned} {
		m.dkim.WithLabelValues(r, source, res)
	}
	for _, p := range providers {
		for _, kind := range []event.Attempted{event.First, event.Retry} {
			m.attempts.WithLabelValues(r, source, p, string(kind))
		}
		m.connectFailures.WithLabelValues(r, source, p)
		m.delivered.WithLabelValues(r, source, p)
		m.complaints.WithLabelValues(r, source, p)
		m.opens.WithLabelValues(r, source, p)
		m.delivery.WithLabelValues(r, p)
		for _, k := range []event.BounceKind{event.Hard, event.Soft, event.Policy} {
			m.bounces.WithLabelValues(r, source, p, string(k))
		}
		for _, a := range []string{labelEngaged, labelAll} {
			m.audience.WithLabelValues(r, source, p, a)
		}
		for _, c := range classify.Categories {
			m.categories.WithLabelValues(r, source, p, string(c))
		}
	}
	for _, spf := range []event.Result{event.Pass, event.Fail} {
		for _, dkim := range []event.Result{event.Pass, event.Fail} {
			m.dmarc.WithLabelValues(source, string(spf), string(dkim))
		}
	}
	for _, t := range event.Types {
		m.events.WithLabelValues(string(t), resultOK)
		m.events.WithLabelValues(string(t), resultInvalid)
	}
}

// label returns the code label a reply counts under in lane k, adding it
// to the set when the bounds allow.
func (c *codeSet) label(k laneKey, e *event.Event) string {
	l := e.CodeLabelWithTag()
	if e.Stage.ConnectionLevel() || knownCodes[l] {
		return l
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	used := c.lanes[k]
	if used == nil {
		used = map[string]bool{}
		c.lanes[k] = used
	}
	bare := strconv.Itoa(e.Code)
	if l != bare {
		if used[l] {
			return l
		}
		if c.detailed[k] < MaxCodesPerLane && c.total < MaxCodeSeries {
			used[l] = true
			c.detailed[k]++
			c.total++
			return l
		}
	}
	if used[bare] {
		return bare
	}
	if c.total < MaxCodeSeries {
		used[bare] = true
		c.total++
		return bare
	}
	return string(e.Class())
}
