package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/cauebraz/ehlo/internal/warmup"
)

// Warmup exports the warmup lanes at scrape time, so a lane's series
// always match what the limits API answers.
type Warmup struct {
	lanes                                             func() []warmup.Decision
	limit, hourly, sent, target, day, backoffs, share *prometheus.Desc
	delivered, bounces, complaints                    *prometheus.Desc
	state                                             *prometheus.Desc
}

// NewWarmup returns a collector that reads the lanes on every scrape.
func NewWarmup(lanes func() []warmup.Decision) *Warmup {
	l := []string{"source", "provider"}
	d := func(name, help string, extra ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, append(l, extra...), nil)
	}
	return &Warmup{
		lanes:      lanes,
		limit:      d("ehlo_warmup_daily_limit_messages", "Today's limit for the lane."),
		hourly:     d("ehlo_warmup_hourly_limit_messages", "The daily limit spread over hours_per_day."),
		sent:       d("ehlo_warmup_sent_today_messages", "First attempts counted today."),
		target:     d("ehlo_warmup_target_messages", "Daily volume the lane grows toward."),
		day:        d("ehlo_warmup_day", "Policy days since the lane was created."),
		backoffs:   d("ehlo_warmup_backoffs", "Backoffs today."),
		share:      d("ehlo_warmup_share_ratio", "The lane's limit over all limits toward the provider."),
		delivered:  d("ehlo_warmup_delivered_today_messages", "Deliveries the policy counted today."),
		bounces:    d("ehlo_warmup_bounces_today", "Bounces the policy counted today."),
		complaints: d("ehlo_warmup_complaints_today", "Complaints the policy counted today."),
		state:      d("ehlo_warmup_state", "1 for the lane's current state.", "state"),
	}
}

// Describe sends every descriptor.
func (w *Warmup) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{w.limit, w.hourly, w.sent, w.target, w.day, w.backoffs, w.share, w.delivered, w.bounces, w.complaints, w.state} {
		ch <- d
	}
}

// Collect reads the lanes and emits one sample per series.
func (w *Warmup) Collect(ch chan<- prometheus.Metric) {
	lanes := w.lanes()
	totals := map[string]float64{}
	for _, d := range lanes {
		totals[d.Provider] += d.Limit
	}
	g := func(desc *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, labels...)
	}
	for _, d := range lanes {
		g(w.limit, d.Limit, d.Source, d.Provider)
		g(w.hourly, d.HourlyLimit, d.Source, d.Provider)
		g(w.sent, d.Sent, d.Source, d.Provider)
		g(w.target, d.Target, d.Source, d.Provider)
		g(w.day, float64(d.Day), d.Source, d.Provider)
		g(w.backoffs, float64(d.Backoffs), d.Source, d.Provider)
		g(w.delivered, d.DeliveredToday, d.Source, d.Provider)
		g(w.bounces, d.BouncesToday, d.Source, d.Provider)
		g(w.complaints, d.ComplaintsToday, d.Source, d.Provider)
		share := 0.0
		if totals[d.Provider] > 0 {
			share = d.Limit / totals[d.Provider]
		}
		g(w.share, share, d.Source, d.Provider)
		for _, st := range warmup.States() {
			v := 0.0
			if st == d.State {
				v = 1
			}
			g(w.state, v, d.Source, d.Provider, string(st))
		}
	}
}
