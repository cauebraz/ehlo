// Package warmup decides how much each sending identity may send to each
// mailbox provider per day, from the delivery signals it observes.
package warmup

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"time"
)

// DefaultKey is the Growth and Shaping entry that covers providers not
// listed by name.
const DefaultKey = "default"

// Policy is the warmup rule set. A config file is decoded over
// DefaultPolicy, so it names only what it changes.
type Policy struct {
	// DecisionHour is the UTC hour at which a new day starts for every lane.
	DecisionHour int `yaml:"decision_hour" json:"decision_hour"`
	// StartLimit is the first daily limit of a new lane.
	StartLimit float64 `yaml:"start_limit" json:"start_limit"`
	// Growth is the daily multiplier per provider. The "default" key covers
	// providers not listed.
	Growth map[string]float64 `yaml:"growth" json:"growth"`
	// HoldComplaintRatio holds the limit when complaints over delivered
	// reach it. PauseComplaintRatio pauses the lane for PauseDays.
	HoldComplaintRatio  float64 `yaml:"hold_complaint_ratio" json:"hold_complaint_ratio"`
	PauseComplaintRatio float64 `yaml:"pause_complaint_ratio" json:"pause_complaint_ratio"`
	// HoldBounceRatio holds the limit when bounces over attempts reach it.
	HoldBounceRatio float64 `yaml:"hold_bounce_ratio" json:"hold_bounce_ratio"`
	// MinSample is the number of deliveries the trailing window needs before
	// its ratios count. Below it the absolute complaint counts apply.
	MinSample float64 `yaml:"min_sample" json:"min_sample"`
	PauseDays int     `yaml:"pause_days" json:"pause_days"`
	// EngagedFirst asks the sender to pick recently engaged recipients while
	// a lane is below its target.
	EngagedFirst *bool `yaml:"engaged_first" json:"engaged_first"`
	// ResumeFraction is the share of the last good limit a lane resumes at
	// after a pause or a block.
	ResumeFraction float64 `yaml:"resume_fraction" json:"resume_fraction"`
	// IdleDaysReset sends a lane back to StartLimit after this many policy
	// days without a first attempt. 0 disables it.
	IdleDaysReset int `yaml:"idle_days_reset" json:"idle_days_reset"`
	// HoursPerDay spreads a warming lane's daily limit into an hourly
	// limit: the sender may use Limit / HoursPerDay in any one policy hour
	// (DayLength / 24). 16 lets a daytime shape peak at 1.5x the flat rate.
	// A lane at target has no hourly cap.
	HoursPerDay int `yaml:"hours_per_day" json:"hours_per_day"`
	// Backoff reacts within the day to rate limiting and reputation blocks.
	Backoff Backoff `yaml:"backoff" json:"backoff"`
	// Shaping is advisory per-provider connection guidance returned with
	// every decision. The "default" key covers providers not listed.
	Shaping map[string]Shaping `yaml:"shaping" json:"shaping"`
	// GrowWhenUsed is the share of the limit a lane must have sent for its
	// limit to grow the next day. Below it the lane holds. 0 grows on any
	// clean day.
	GrowWhenUsed float64 `yaml:"grow_when_used" json:"grow_when_used"`
	// HoldComplaints and PauseComplaints are absolute daily counts that hold
	// or pause a lane while the window is below MinSample.
	HoldComplaints  float64 `yaml:"hold_complaints" json:"hold_complaints"`
	PauseComplaints float64 `yaml:"pause_complaints" json:"pause_complaints"`
	// TrailingDays is how many closed days the complaint and bounce ratios
	// span, so feedback that arrives a day late still counts.
	TrailingDays int `yaml:"trailing_days" json:"trailing_days"`
	// DayLength is 24h in production. Demos and tests shorten it.
	DayLength time.Duration `yaml:"day_length" json:"day_length"`
}

// Shaping is what an MTA operator sets per provider: how many connections
// to open and how many messages to send on each. Seeded from the limits
// the providers publish and the KumoMTA community shaping file.
type Shaping struct {
	Connections   int `yaml:"connections" json:"connections"`
	PerConnection int `yaml:"per_connection" json:"per_connection"`
}

// DefaultPolicy is the policy ehlo ships with. Each call returns a fresh
// copy, so a caller may edit the maps.
func DefaultPolicy() Policy {
	yes := true
	return Policy{
		StartLimit:          50,
		Growth:              map[string]float64{DefaultKey: 1.4, "gmail": 1.5, "outlook": 1.3, "yahoo": 1.35},
		HoldComplaintRatio:  0.001,
		PauseComplaintRatio: 0.003,
		HoldBounceRatio:     0.03,
		MinSample:           50,
		PauseDays:           1,
		EngagedFirst:        &yes,
		ResumeFraction:      0.5,
		IdleDaysReset:       30,
		HoursPerDay:         16,
		Backoff:             Backoff{Enabled: &yes, Window: 5 * time.Minute, MinAttempts: 20, Ratio: 0.05, Duration: 10 * time.Minute, BlockRatio: 0.01, MinHits: 5},
		Shaping: map[string]Shaping{
			DefaultKey: {Connections: 10, PerConnection: 100},
			"gmail":    {Connections: 5, PerConnection: 50},
			"outlook":  {Connections: 5, PerConnection: 50},
			"yahoo":    {Connections: 10, PerConnection: 20},
			"icloud":   {Connections: 10, PerConnection: 5},
		},
		GrowWhenUsed:    0.7,
		HoldComplaints:  2,
		PauseComplaints: 3,
		TrailingDays:    3,
		DayLength:       24 * time.Hour,
	}
}

// With returns a copy of the policy changed by fn. Tests and demos use it
// over DefaultPolicy.
func (p Policy) With(fn func(*Policy)) Policy {
	p.Growth = maps.Clone(p.Growth)
	p.Shaping = maps.Clone(p.Shaping)
	fn(&p)
	return p
}

// lookup returns the provider's entry or the default one.
func lookup[V any](m map[string]V, provider string) V {
	if v, ok := m[provider]; ok {
		return v
	}
	return m[DefaultKey]
}

// Validate returns every problem in the policy at once.
func (p Policy) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	for name, v := range map[string]float64{
		"start_limit": p.StartLimit, "hold_complaint_ratio": p.HoldComplaintRatio, "pause_complaint_ratio": p.PauseComplaintRatio,
		"hold_bounce_ratio": p.HoldBounceRatio, "min_sample": p.MinSample, "resume_fraction": p.ResumeFraction,
		"grow_when_used": p.GrowWhenUsed, "hold_complaints": p.HoldComplaints, "pause_complaints": p.PauseComplaints,
		"backoff.ratio": p.Backoff.Ratio, "backoff.block_ratio": p.Backoff.BlockRatio,
		"backoff.min_attempts": p.Backoff.MinAttempts, "backoff.min_hits": p.Backoff.MinHits,
	} {
		if !finite(v) {
			fail("%s = %v, must be a finite number", name, v)
		}
	}
	for k, g := range p.Growth {
		if !finite(g) {
			fail("growth.%s = %v, must be a finite number", k, g)
		}
	}
	if p.DecisionHour < 0 || p.DecisionHour > 23 {
		fail("decision_hour %d outside 0-23", p.DecisionHour)
	}
	if p.StartLimit <= 0 {
		fail("start_limit must be positive")
	}
	if _, ok := p.Growth[DefaultKey]; !ok {
		fail("growth.default is required")
	}
	for k, g := range p.Growth {
		if g <= 1 {
			fail("growth.%s = %v, must be above 1", k, g)
		}
	}
	if p.HoldComplaintRatio <= 0 || p.PauseComplaintRatio <= p.HoldComplaintRatio {
		fail("pause_complaint_ratio must be above hold_complaint_ratio, both positive")
	}
	if p.HoldBounceRatio <= 0 {
		fail("hold_bounce_ratio must be positive")
	}
	if p.MinSample < 1 {
		fail("min_sample must be at least 1")
	}
	if p.PauseDays < 1 {
		fail("pause_days must be at least 1")
	}
	if p.EngagedFirst == nil {
		fail("engaged_first must be set")
	}
	if p.ResumeFraction <= 0 || p.ResumeFraction > 1 {
		fail("resume_fraction must be in (0, 1]")
	}
	if p.IdleDaysReset < 0 {
		fail("idle_days_reset must not be negative")
	}
	if p.HoursPerDay < 1 || p.HoursPerDay > 24 {
		fail("hours_per_day must be 1 to 24")
	}
	if p.Backoff.Enabled == nil {
		fail("backoff.enabled must be set")
	}
	errs = append(errs, p.Backoff.validate()...)
	if _, ok := p.Shaping[DefaultKey]; !ok {
		fail("shaping.default is required")
	}
	for k, sh := range p.Shaping {
		if sh.Connections < 1 || sh.PerConnection < 1 {
			fail("shaping.%s needs connections and per_connection >= 1", k)
		}
	}
	if p.GrowWhenUsed < 0 || p.GrowWhenUsed > 1 {
		fail("grow_when_used must be in [0, 1]")
	}
	if p.HoldComplaints < 1 || p.PauseComplaints <= p.HoldComplaints {
		fail("pause_complaints must be above hold_complaints, both at least 1")
	}
	if p.TrailingDays < 1 || p.TrailingDays > 14 {
		fail("trailing_days must be 1 to 14")
	}
	if p.DayLength < time.Minute || p.DayLength > 24*time.Hour {
		fail("day_length must be between 1m and 24h")
	}
	return errors.Join(errs...)
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// DayStart returns the start of the policy day that contains t. With a
// DayLength under 24h, days are DayLength-long slots counted from the
// Unix epoch.
func (p Policy) DayStart(t time.Time) time.Time {
	t = t.UTC()
	if p.DayLength > 0 && p.DayLength < 24*time.Hour {
		return t.Truncate(p.DayLength)
	}
	start := time.Date(t.Year(), t.Month(), t.Day(), p.DecisionHour, 0, 0, 0, time.UTC)
	if t.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	return start
}

// HourLength is the slot the hourly limit applies to: one 24th of
// DayLength, an hour on a 24h day.
func (p Policy) HourLength() time.Duration {
	return p.DayLength / 24
}

// HourStart returns the start of the hour slot that contains t, counted
// from dayStart so a slot never straddles two policy days.
func (p Policy) HourStart(dayStart, t time.Time) time.Time {
	if t.Before(dayStart) {
		return dayStart
	}
	h := p.HourLength()
	return dayStart.Add(t.Sub(dayStart) / h * h)
}
