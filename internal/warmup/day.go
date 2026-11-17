package warmup

import "fmt"

// verdict is what closing a day decides for a lane.
type verdict struct {
	state    State
	limit    float64
	reason   string
	lastGood float64
}

func ratio(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return a / b
}

// trailing sums the closed days the ratios span.
func (l *Lane) trailing() dayStats {
	var win dayStats
	for _, d := range l.History {
		win = win.add(d)
	}
	return win
}

func (p Policy) resumeLimit(l *Lane) float64 {
	return min(l.Target, max(p.StartLimit, l.LastGood*p.ResumeFraction))
}

func complaintReason(win dayStats, days int) string {
	return fmt.Sprintf("%.0f complaints, %.2f%% of delivered over %d days", win.Complaints, ratio(win.Complaints, win.Delivered)*100, days)
}

// verdict picks the next state of a lane from its finished day and its
// trailing window. The cases are ordered by precedence: an idle reset beats
// a pause, a pause beats a block, a block beats a hold, and growth comes
// last. It reads the lane and writes nothing.
func (p Policy) verdict(l *Lane, today, win dayStats) verdict {
	days := len(l.History)
	complaintRatio := ratio(win.Complaints, win.Delivered)
	bounceRatio := ratio(win.Bounces, win.Sent)
	enough := win.Delivered >= p.MinSample
	keep := l.LastGood
	switch {
	case p.IdleDaysReset > 0 && l.IdleDays >= p.IdleDaysReset && l.Target > p.StartLimit:
		return verdict{Raising, p.StartLimit, fmt.Sprintf("no traffic for %d days, back to start_limit", p.IdleDaysReset), p.StartLimit}
	case l.State == Paused:
		if l.PausedFor+1 >= p.PauseDays {
			return verdict{Holding, p.resumeLimit(l), fmt.Sprintf("resumed at %.0f%% of the last good limit after the pause", p.ResumeFraction*100), keep}
		}
		return verdict{Paused, 0, l.Reason, keep}
	case (!enough && today.Complaints >= p.PauseComplaints) || (enough && complaintRatio >= p.PauseComplaintRatio):
		return verdict{Paused, 0, complaintReason(win, days), l.Limit}
	case !l.BlockedUntil.IsZero():
		return verdict{Holding, p.resumeLimit(l), "blocked by the provider yesterday", keep}
	case (!enough && today.Complaints >= p.HoldComplaints) || (enough && complaintRatio >= p.HoldComplaintRatio):
		return verdict{Holding, l.Limit, complaintReason(win, days), keep}
	case enough && bounceRatio >= p.HoldBounceRatio:
		return verdict{Holding, l.Limit, fmt.Sprintf("bounces at %.1f%% of attempts over %d days", bounceRatio*100, days), keep}
	case l.BackedOff:
		return verdict{Holding, l.Limit, "backed off yesterday", keep}
	case l.Limit >= l.Target:
		return verdict{AtTarget, l.Target, "at target", l.Target}
	case today.Sent < p.GrowWhenUsed*l.Limit:
		return verdict{Holding, l.Limit, fmt.Sprintf("sent %.0f of %.0f yesterday, below %.0f%% of the limit", today.Sent, l.Limit, p.GrowWhenUsed*100), keep}
	}
	g := lookup(p.Growth, l.Provider)
	return verdict{Raising, min(l.Target, max(l.Limit, today.Sent)*g), fmt.Sprintf("clean day, limit x%.2f", g), l.Limit}
}

// closeDay applies the policy to one finished day of a lane. Complaint and
// bounce ratios span the trailing days so feedback that arrives a day late
// still counts; the absolute complaint counts cover the first days, when
// the sample is too small for a ratio.
func (p Policy) closeDay(l *Lane) {
	today := l.today()
	l.History = append(l.History, today)
	if len(l.History) > p.TrailingDays {
		l.History = l.History[len(l.History)-p.TrailingDays:]
	}
	if today.Sent == 0 {
		l.IdleDays++
	} else {
		l.IdleDays = 0
	}
	v := p.verdict(l, today, l.trailing())
	switch {
	case v.state == Paused && l.State == Paused:
		l.PausedFor++
	case v.state == Paused:
		l.PausedFor = 0
	case l.State == Paused:
		l.PausedFor = 0
		l.History = nil
	}
	if p.IdleDaysReset > 0 && l.IdleDays >= p.IdleDaysReset && l.Target > p.StartLimit {
		l.IdleDays = 0
	}
	l.State, l.Limit, l.Reason, l.LastGood = v.state, v.limit, v.reason, v.lastGood
	l.startDay()
}
