package warmup

import (
	"fmt"
	"time"
)

// backoffHourlyFactor is applied to the hourly allowance on each backoff.
const backoffHourlyFactor = 0.5

// Backoff stops a lane for Duration when, over Window, at least MinAttempts
// were made and the share answered with rate_limited or reputation_block
// replies reached Ratio. A backed-off lane also holds its growth that day.
type Backoff struct {
	Enabled     *bool         `yaml:"enabled" json:"enabled"`
	Window      time.Duration `yaml:"window" json:"window"`
	MinAttempts float64       `yaml:"min_attempts" json:"min_attempts"`
	Ratio       float64       `yaml:"ratio" json:"ratio"`
	Duration    time.Duration `yaml:"duration" json:"duration"`
	// BlockRatio is the share of reputation_block replies that ends the
	// lane's day. The next day opens at ResumeFraction of the last limit.
	BlockRatio float64 `yaml:"block_ratio" json:"block_ratio"`
	// MinHits is the number of rate limiting or block replies a window
	// needs before the ratio counts, so one reply among few attempts does
	// not stop a lane.
	MinHits float64 `yaml:"min_hits" json:"min_hits"`
}

func (b Backoff) validate() []error {
	var errs []error
	if b.Window <= 0 || b.Duration <= 0 {
		errs = append(errs, fmt.Errorf("backoff.window and backoff.duration must be positive"))
	}
	if b.Ratio <= 0 || b.Ratio > 1 {
		errs = append(errs, fmt.Errorf("backoff.ratio must be in (0, 1]"))
	}
	if b.BlockRatio <= 0 || b.BlockRatio > 1 {
		errs = append(errs, fmt.Errorf("backoff.block_ratio must be in (0, 1]"))
	}
	if b.MinAttempts < 1 || b.MinHits < 1 {
		errs = append(errs, fmt.Errorf("backoff.min_attempts and backoff.min_hits must be at least 1"))
	}
	return errs
}

func (b Backoff) blockReason() string {
	return fmt.Sprintf("reputation block replies reached %.0f%% of attempts, stopped until the next day", b.BlockRatio*100)
}

func (b Backoff) throttleReason() string {
	return fmt.Sprintf("rate limiting replies reached %.0f%% of attempts in %s, stopped for %s", b.Ratio*100, b.Window, b.Duration)
}

// hourView is the hour slot and the messages sent in it as of now, without
// writing to the lane.
func (p Policy) hourView(l *Lane, now time.Time) (time.Time, float64) {
	hour := p.HourStart(l.DayStart, now.UTC())
	if l.HourStart.IsZero() || hour.After(l.HourStart) {
		return hour, 0
	}
	return l.HourStart, l.SentHour
}

func (p Policy) rollHour(l *Lane, now time.Time) {
	l.HourStart, l.SentHour = p.hourView(l, now)
}

func (p Policy) rollWindow(l *Lane, now time.Time) {
	if l.WindowStart.IsZero() || now.Sub(l.WindowStart) >= p.Backoff.Window {
		l.resetWindow(now)
	}
}

// react applies the intraday rules: a reputation block ends the day, rate
// limiting stops the lane for Backoff.Duration and halves the hourly
// allowance for the rest of the day.
func (p Policy) react(l *Lane, now time.Time) {
	b := p.Backoff
	if !*b.Enabled || l.WindowAttempts < b.MinAttempts || now.Before(l.BlockedUntil) {
		return
	}
	if l.WindowBlock >= b.MinHits && l.WindowBlock/l.WindowAttempts >= b.BlockRatio {
		l.BlockedUntil = l.DayStart.Add(p.DayLength)
		l.BackedOff = true
		l.Backoffs++
		return
	}
	if now.Before(l.BackoffUntil) || l.WindowThrottle < b.MinHits || l.WindowThrottle/l.WindowAttempts < b.Ratio {
		return
	}
	l.BackoffUntil = now.Add(b.Duration)
	l.HourlyFactor *= backoffHourlyFactor
	l.BackedOff = true
	l.Backoffs++
	l.resetWindow(now)
}
