package warmup

import (
	"math"
	"strings"
	"testing"
)

func TestValidateRejectsNonFiniteNumbers(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	policies := map[string]func(*Policy){
		"growth.default":        func(p *Policy) { p.Growth[DefaultKey] = nan },
		"growth.gmail":          func(p *Policy) { p.Growth["gmail"] = inf },
		"start_limit":           func(p *Policy) { p.StartLimit = inf },
		"hold_complaint_ratio":  func(p *Policy) { p.HoldComplaintRatio = nan },
		"pause_complaint_ratio": func(p *Policy) { p.PauseComplaintRatio = inf },
		"hold_bounce_ratio":     func(p *Policy) { p.HoldBounceRatio = inf },
		"min_sample":            func(p *Policy) { p.MinSample = inf },
		"resume_fraction":       func(p *Policy) { p.ResumeFraction = nan },
		"grow_when_used":        func(p *Policy) { p.GrowWhenUsed = nan },
		"hold_complaints":       func(p *Policy) { p.HoldComplaints = nan },
		"pause_complaints":      func(p *Policy) { p.PauseComplaints = inf },
		"backoff.ratio":         func(p *Policy) { p.Backoff.Ratio = nan },
		"backoff.block_ratio":   func(p *Policy) { p.Backoff.BlockRatio = nan },
		"backoff.min_attempts":  func(p *Policy) { p.Backoff.MinAttempts = inf },
		"backoff.min_hits":      func(p *Policy) { p.Backoff.MinHits = nan },
	}
	for field, mod := range policies {
		err := DefaultPolicy().With(mod).Validate()
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: Validate() = %v, want an error naming it", field, err)
		}
	}
	targets := map[string]Targets{
		"targets.default":      {Default: inf},
		"targets.sources.mx-1": {Sources: map[string]SourceTarget{"mx-1": {Daily: nan}}},
		"targets.mix.gmail":    {Mix: map[string]float64{"gmail": nan}},
	}
	for field, tg := range targets {
		err := tg.Validate()
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: Validate() = %v, want an error naming it", field, err)
		}
	}
}
