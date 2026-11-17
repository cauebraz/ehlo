package warmup

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

// mixTolerance absorbs float noise when the mix shares are summed.
const mixTolerance = 1.0001

// Targets name the daily volume each lane grows toward.
type Targets struct {
	// Default is the daily target of a source not listed in Sources.
	Default float64 `yaml:"default" json:"default"`
	// Sources overrides the daily target per source. A target of 0 means the
	// source is not managed and every query returns no limit.
	Sources map[string]SourceTarget `yaml:"sources" json:"sources"`
	// Mix splits a source's target across providers. A provider not listed
	// may use the whole remainder: the remainder is a cap for the long tail,
	// not a budget to divide, because nothing says how that tail splits.
	Mix map[string]float64 `yaml:"mix" json:"mix"`
	// Warm lists sources that start at their target instead of StartLimit.
	Warm []string `yaml:"warm" json:"warm"`
}

// SourceTarget is one source's entry in Targets.Sources.
type SourceTarget struct {
	Daily float64 `yaml:"daily" json:"daily"`
	Warm  bool    `yaml:"warm" json:"warm"`
}

// Validate returns every problem in the targets at once.
func (t Targets) Validate() error {
	var errs []error
	if t.Default < 0 || !finite(t.Default) {
		errs = append(errs, errors.New("targets.default must be a finite number, not negative"))
	}
	for name, st := range t.Sources {
		if st.Daily < 0 || !finite(st.Daily) {
			errs = append(errs, fmt.Errorf("targets.sources.%s.daily must be a finite number, not negative", name))
		}
	}
	var sum float64
	for p, m := range t.Mix {
		if m < 0 || m > 1 || !finite(m) {
			errs = append(errs, fmt.Errorf("targets.mix.%s = %v outside 0-1", p, m))
		}
		sum += m
	}
	if sum > mixTolerance {
		errs = append(errs, fmt.Errorf("targets.mix sums to %.3f, above 1", sum))
	}
	return errors.Join(errs...)
}

func (t Targets) daily(source string) float64 {
	if s, ok := t.Sources[source]; ok {
		return s.Daily
	}
	return t.Default
}

// known reports whether the config names the source.
func (t Targets) known(source string) bool {
	_, ok := t.Sources[source]
	return ok || t.warm(source)
}

func (t Targets) warm(source string) bool {
	if s, ok := t.Sources[source]; ok && s.Warm {
		return true
	}
	return slices.Contains(t.Warm, source)
}

// lane is the target for one provider: the explicit mix share, or the whole
// remainder for a provider without one.
func (t Targets) lane(source, provider string, providers map[string]struct{}) float64 {
	daily := t.daily(source)
	if m, ok := t.Mix[provider]; ok {
		return round6(daily * m)
	}
	var listed float64
	for p := range providers {
		if m, ok := t.Mix[p]; ok {
			listed += m
		}
	}
	return round6(daily * max(0, 1-listed))
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
