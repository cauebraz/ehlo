package warmup

import (
	"cmp"
	"strings"
	"time"
)

// State is where a lane is in its warmup.
type State string

const (
	// AtTarget means the lane sends its full target.
	AtTarget State = "at_target"
	// Raising means the limit grows each clean day.
	Raising State = "raising"
	// Holding means the limit stays where it is because of complaints,
	// bounces, low volume or a backoff the day before.
	Holding State = "holding"
	// Paused means complaints crossed the pause ratio, limit 0 for pause_days.
	Paused State = "paused"
	// BackedOff means rate limiting replies crossed the backoff ratio, send
	// nothing until resume_at.
	BackedOff State = "backoff"
	// Blocked means the provider blocked the source for reputation, send
	// nothing for the rest of the day.
	Blocked State = "blocked"
)

// States lists every state, in the order the dashboards show them.
func States() []State {
	return []State{AtTarget, Raising, Holding, Paused, BackedOff, Blocked}
}

// dayStats is one closed day of a lane, kept for the trailing ratios.
type dayStats struct {
	Sent       float64 `json:"sent"`
	Delivered  float64 `json:"delivered"`
	Bounces    float64 `json:"bounces"`
	Complaints float64 `json:"complaints"`
}

func (d dayStats) add(o dayStats) dayStats {
	return dayStats{Sent: d.Sent + o.Sent, Delivered: d.Delivered + o.Delivered, Bounces: d.Bounces + o.Bounces, Complaints: d.Complaints + o.Complaints}
}

// Lane is the warmup state of one source toward one provider.
type Lane struct {
	Source   string  `json:"source"`
	Provider string  `json:"provider"`
	Limit    float64 `json:"limit"`
	Target   float64 `json:"target"`
	State    State   `json:"state"`
	Reason   string  `json:"reason,omitempty"`
	// Day counts policy days since the lane was created.
	Day       int       `json:"day"`
	LastGood  float64   `json:"last_good"`
	PausedFor int       `json:"paused_for,omitempty"`
	DayStart  time.Time `json:"day_start"`

	Sent       float64 `json:"sent"`
	Delivered  float64 `json:"delivered"`
	Bounces    float64 `json:"bounces"`
	Complaints float64 `json:"complaints"`
	// History holds the last TrailingDays closed days, newest last.
	History []dayStats `json:"history,omitempty"`

	HourStart    time.Time `json:"hour_start"`
	SentHour     float64   `json:"sent_hour"`
	HourlyFactor float64   `json:"hourly_factor,omitempty"`
	IdleDays     int       `json:"idle_days,omitempty"`

	// Backoff window: attempts and throttling replies since WindowStart.
	WindowStart    time.Time `json:"window_start"`
	WindowAttempts float64   `json:"window_attempts"`
	WindowThrottle float64   `json:"window_throttle"`
	WindowBlock    float64   `json:"window_block"`
	BackoffUntil   time.Time `json:"backoff_until"`
	BlockedUntil   time.Time `json:"blocked_until"`
	BackedOff      bool      `json:"backed_off,omitempty"`
	Backoffs       int       `json:"backoffs,omitempty"`
}

func (l *Lane) today() dayStats {
	return dayStats{Sent: l.Sent, Delivered: l.Delivered, Bounces: l.Bounces, Complaints: l.Complaints}
}

func (l *Lane) resetWindow(now time.Time) {
	l.WindowStart, l.WindowAttempts, l.WindowThrottle, l.WindowBlock = now, 0, 0, 0
}

// startDay clears the counters a new policy day begins with.
func (l *Lane) startDay() {
	l.Day++
	l.Sent, l.Delivered, l.Bounces, l.Complaints = 0, 0, 0, 0
	l.BackedOff, l.Backoffs, l.HourlyFactor = false, 0, 1
	l.BlockedUntil = time.Time{}
}

func compareLanes(a, b *Lane) int {
	return cmp.Or(strings.Compare(a.Source, b.Source), strings.Compare(a.Provider, b.Provider))
}

// Decision is what a sender needs before routing a message.
type Decision struct {
	Source   string `json:"source"`
	Provider string `json:"provider"`
	Managed  bool   `json:"managed"`
	State    State  `json:"state"`
	// Reason says why the lane is in its state, for people and logs.
	Reason string `json:"reason,omitempty"`
	// AllowedNow is how many messages the lane may send right now: the
	// smaller of the daily and hourly remainders, 0 while backed off or
	// blocked. A router gates on this field.
	AllowedNow float64 `json:"allowed_now"`
	// ResumeAt is set while AllowedNow is 0 for a reason that ends at a
	// known time.
	ResumeAt *time.Time `json:"resume_at,omitempty"`

	Limit     float64 `json:"limit"`
	Sent      float64 `json:"sent"`
	Remaining float64 `json:"remaining"`
	Target    float64 `json:"target"`
	Day       int     `json:"day"`

	HourlyLimit   float64   `json:"hourly_limit"`
	SentHour      float64   `json:"sent_hour"`
	RemainingHour float64   `json:"remaining_hour"`
	NextHourAt    time.Time `json:"next_hour_at"`

	// Today's counters as the policy sees them.
	DeliveredToday  float64 `json:"delivered_today"`
	BouncesToday    float64 `json:"bounces_today"`
	ComplaintsToday float64 `json:"complaints_today"`

	EngagedOnly bool `json:"engaged_only"`
	Backoffs    int  `json:"backoffs"`
	// Shaping is the advisory connection guidance for the provider.
	Shaping Shaping `json:"shaping"`

	DayStart   time.Time `json:"day_start"`
	DayEnd     time.Time `json:"day_end"`
	ServerTime time.Time `json:"server_time"`
}

func compareDecisions(a, b Decision) int {
	return cmp.Or(strings.Compare(a.Source, b.Source), strings.Compare(a.Provider, b.Provider))
}

// decision reads a lane without changing it. The hour view is computed, not
// stored, so a scrape or a limits query never writes.
func (p Policy) decision(l *Lane, now time.Time) Decision {
	hourStart, sentHour := p.hourView(l, now)
	hourly := l.Limit
	if l.Limit < l.Target || l.HourlyFactor < 1 {
		hourly = l.Limit / float64(p.HoursPerDay) * l.HourlyFactor
	}
	remaining := max(0, l.Limit-l.Sent)
	remainingHour := max(0, min(hourly-sentHour, remaining))
	d := Decision{
		Source:          l.Source,
		Provider:        l.Provider,
		Managed:         true,
		State:           l.State,
		Reason:          l.Reason,
		AllowedNow:      remainingHour,
		Limit:           l.Limit,
		Sent:            l.Sent,
		Remaining:       remaining,
		Target:          l.Target,
		Day:             l.Day,
		HourlyLimit:     hourly,
		SentHour:        sentHour,
		RemainingHour:   remainingHour,
		NextHourAt:      hourStart.Add(p.HourLength()),
		EngagedOnly:     *p.EngagedFirst && l.Limit < l.Target,
		Backoffs:        l.Backoffs,
		DeliveredToday:  l.Delivered,
		BouncesToday:    l.Bounces,
		ComplaintsToday: l.Complaints,
		Shaping:         lookup(p.Shaping, l.Provider),
		DayStart:        l.DayStart,
		DayEnd:          l.DayStart.Add(p.DayLength),
		ServerTime:      now,
	}
	switch {
	case now.Before(l.BlockedUntil):
		d.stop(Blocked, l.BlockedUntil, p.Backoff.blockReason())
	case now.Before(l.BackoffUntil):
		d.stop(BackedOff, l.BackoffUntil, p.Backoff.throttleReason())
	case l.State == Paused:
		next := l.DayStart.Add(p.DayLength)
		d.ResumeAt = &next
	}
	return d
}

func (d *Decision) stop(st State, until time.Time, reason string) {
	d.State, d.AllowedNow, d.ResumeAt, d.Reason = st, 0, &until, reason
}
