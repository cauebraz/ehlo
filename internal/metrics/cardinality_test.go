package metrics_test

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cauebraz/ehlo/internal/classify"
	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/metrics"
)

func TestCountFoldsUnknownTypes(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := metrics.New(reg, "test")
	for i := range 100 {
		m.Count(event.Type(fmt.Sprintf("bogus-%d", i)), false)
	}
	if got := count(t, reg, "ehlo_events_total"); got != 1 {
		t.Errorf("ehlo_events_total series = %d, want 1 for 100 unknown types", got)
	}
	if got := value(t, reg, "ehlo_events_total", map[string]string{"type": "unknown", "result": "invalid"}); got != 100 {
		t.Errorf(`ehlo_events_total{type="unknown",result="invalid"} = %v, want 100`, got)
	}
}

func flood(m *metrics.Metrics, source string, n int) {
	for i := range n {
		e := base(event.Reply)
		e.Source = source
		e.Code, e.Enhanced = 451, fmt.Sprintf("4.%d.%d", i/1000, i%1000)
		m.Observe(e, classify.Other)
	}
}

func TestReplyCodesAreBoundedPerLane(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := metrics.New(reg, "test")
	flood(m, "mx-1", 3*metrics.MaxCodesPerLane)
	if got := count(t, reg, "ehlo_smtp_replies_total"); got != metrics.MaxCodesPerLane+1 {
		t.Errorf("ehlo_smtp_replies_total series = %d, want %d", got, metrics.MaxCodesPerLane+1)
	}
	if got, want := value(t, reg, "ehlo_smtp_replies_total", lane("class", "4xx", "code", "451")), float64(2*metrics.MaxCodesPerLane); got != want {
		t.Errorf(`code="451" = %v, want %v replies past the lane bound`, got, want)
	}
}

func TestReplySeriesAreBoundedOverall(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := metrics.New(reg, "test")
	sources := 2*metrics.MaxCodeSeries/metrics.MaxCodesPerLane + 1
	for s := range sources {
		src := fmt.Sprintf("mx-%d", s)
		flood(m, src, metrics.MaxCodesPerLane)
		for code := 200; code < 600; code++ {
			if code >= 300 && code < 400 {
				continue
			}
			e := base(event.Reply)
			e.Source, e.Code = src, code
			m.Observe(e, classify.Other)
		}
	}
	if got, limit := count(t, reg, "ehlo_smtp_replies_total"), metrics.MaxCodeSeries+3*sources; got > limit {
		t.Errorf("ehlo_smtp_replies_total series = %d, want at most %d", got, limit)
	}
	last := lane("class", "5xx", "code", "5xx")
	last["source"] = fmt.Sprintf("mx-%d", sources-1)
	if got := value(t, reg, "ehlo_smtp_replies_total", last); got != 100 {
		t.Errorf(`code="5xx" on the last source = %v, want 100 replies past the overall bound`, got)
	}
}

func TestKnownReplyCodesAreKeptPastTheBounds(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := metrics.New(reg, "test")
	for s := range 2*metrics.MaxCodeSeries/metrics.MaxCodesPerLane + 1 {
		flood(m, fmt.Sprintf("mx-%d", s), 2*metrics.MaxCodesPerLane)
	}
	flood(m, "mx-1", 2*metrics.MaxCodesPerLane)
	tss04 := base(event.Reply)
	tss04.Source, tss04.Provider, tss04.Code, tss04.Enhanced, tss04.Tag = "mx-1", "yahoo", 421, "4.7.0", "TSS04"
	m.Observe(tss04, classify.RateLimited)
	labels := lane("class", "4xx", "code", "421 4.7.0 TSS04")
	labels["provider"] = "yahoo"
	if got := value(t, reg, "ehlo_smtp_replies_total", labels); got != 1 {
		t.Errorf(`code="421 4.7.0 TSS04" = %v, want 1: a known code is kept past every bound`, got)
	}
}

func TestReplyCodeLabelsKeepKnownValuesPastTheCap(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := metrics.New(reg, "test")
	first := base(event.Reply)
	first.Code, first.Enhanced = 421, "4.7.1"
	m.Observe(first, classify.Other)
	flood(m, "mx-1", 2*metrics.MaxCodesPerLane)
	m.Observe(first, classify.Other)
	if got := value(t, reg, "ehlo_smtp_replies_total", lane("class", "4xx", "code", "421 4.7.1")); got != 2 {
		t.Errorf(`code="421 4.7.1" = %v, want 2: a label seen before the cap keeps counting`, got)
	}
}

func TestConnectionRepliesPastTheCapKeepTheirStage(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := metrics.New(reg, "test")
	for s := range 2*metrics.MaxCodeSeries/metrics.MaxCodesPerLane + 1 {
		flood(m, fmt.Sprintf("mx-%d", s), metrics.MaxCodesPerLane)
	}
	e := base(event.Reply)
	e.Source, e.Stage = "mx-1", event.StageTLS
	m.Observe(e, classify.Connection)
	if got := value(t, reg, "ehlo_smtp_replies_total", lane("class", "connect", "code", "tls")); got != 1 {
		t.Errorf(`code="tls" = %v, want 1`, got)
	}
}
