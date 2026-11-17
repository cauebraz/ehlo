package warmup

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestVerdict(t *testing.T) {
	p := DefaultPolicy()
	twoDayPause := p.With(func(p *Policy) { p.PauseDays = 2 })
	raising := func(limit, target float64) Lane {
		return Lane{Provider: "gmail", Limit: limit, Target: target, LastGood: limit, State: Raising}
	}
	enough := dayStats{Sent: 1000, Delivered: 1000}

	cases := []struct {
		name   string
		policy Policy
		lane   Lane
		today  dayStats
		win    dayStats
		state  State
		limit  float64
		reason string
	}{
		{"idle reset", p, Lane{Provider: "gmail", Limit: 400, Target: 1000, IdleDays: 30, State: Holding, LastGood: 400}, dayStats{}, dayStats{}, Raising, 50, "no traffic for 30 days"},
		{"idle but at start limit", p, Lane{Provider: "gmail", Limit: 50, Target: 50, IdleDays: 30, State: AtTarget}, dayStats{}, dayStats{}, AtTarget, 50, "at target"},
		{"paused and still paused", twoDayPause, Lane{Provider: "gmail", Limit: 0, Target: 1000, State: Paused, PausedFor: 0, Reason: "3 complaints", LastGood: 400}, dayStats{}, dayStats{}, Paused, 0, "3 complaints"},
		{"paused and resuming", p, Lane{Provider: "gmail", Limit: 0, Target: 1000, State: Paused, LastGood: 400}, dayStats{}, dayStats{}, Holding, 200, "resumed at 50%"},
		{"paused resumes at least start limit", p, Lane{Provider: "gmail", Target: 1000, State: Paused, LastGood: 60}, dayStats{}, dayStats{}, Holding, 50, "resumed at 50%"},
		{"pause by count on a small sample", p, raising(100, 1000), dayStats{Sent: 100, Delivered: 10, Complaints: 3}, dayStats{Sent: 100, Delivered: 10, Complaints: 3}, Paused, 0, "3 complaints, 30.00% of delivered over 0 days"},
		{"pause by ratio", p, raising(100, 1000), dayStats{Sent: 100, Delivered: 100}, dayStats{Sent: 1000, Delivered: 1000, Complaints: 3}, Paused, 0, "3 complaints, 0.30% of delivered"},
		{"pause beats block", p, Lane{Provider: "gmail", Limit: 100, Target: 1000, LastGood: 100, BlockedUntil: day0}, dayStats{}, dayStats{Delivered: 1000, Complaints: 3}, Paused, 0, "complaints"},
		{"blocked yesterday", p, Lane{Provider: "gmail", Limit: 400, Target: 1000, LastGood: 400, BlockedUntil: day0, State: Raising}, dayStats{Sent: 400, Delivered: 300}, dayStats{Sent: 400, Delivered: 300}, Holding, 200, "blocked by the provider yesterday"},
		{"hold by count on a small sample", p, raising(100, 1000), dayStats{Sent: 100, Delivered: 10, Complaints: 2}, dayStats{Sent: 100, Delivered: 10, Complaints: 2}, Holding, 100, "2 complaints"},
		{"hold by ratio", p, raising(100, 1000), dayStats{Sent: 100, Delivered: 100}, dayStats{Sent: 1000, Delivered: 1000, Complaints: 1}, Holding, 100, "1 complaints, 0.10% of delivered"},
		{"count does not pause once the sample is enough", p, raising(100, 1000), dayStats{Sent: 100, Delivered: 100, Complaints: 3}, dayStats{Sent: 10000, Delivered: 10000, Complaints: 3}, Raising, 150, "clean day"},
		{"hold by bounce ratio", p, raising(100, 1000), dayStats{Sent: 100, Delivered: 97}, dayStats{Sent: 1000, Delivered: 970, Bounces: 30}, Holding, 100, "bounces at 3.0% of attempts"},
		{"bounce ratio counts attempts, not deliveries", p, raising(100, 1000), dayStats{Sent: 100, Delivered: 40, Bounces: 10}, dayStats{Sent: 100, Delivered: 40, Bounces: 10}, Holding, 100, "bounces at 10.0% of attempts"},
		{"new lane bouncing almost everything holds", p, raising(50, 1000), dayStats{Sent: 50, Delivered: 2, Bounces: 48}, dayStats{Sent: 50, Delivered: 2, Bounces: 48}, Holding, 50, "bounces at 96.0% of attempts"},
		{"bounce ratio needs the sample of attempts", p, raising(50, 1000), dayStats{Sent: 40, Delivered: 2, Bounces: 38}, dayStats{Sent: 40, Delivered: 2, Bounces: 38}, Raising, 75, "clean day"},
		{"backed off yesterday", p, Lane{Provider: "gmail", Limit: 100, Target: 1000, LastGood: 100, BackedOff: true, State: Raising}, dayStats{Sent: 100, Delivered: 100}, enough, Holding, 100, "backed off yesterday"},
		{"at target", p, Lane{Provider: "gmail", Limit: 1000, Target: 1000, LastGood: 1000, State: AtTarget}, dayStats{Sent: 1000, Delivered: 1000}, enough, AtTarget, 1000, "at target"},
		{"above target clamps to target", p, Lane{Provider: "gmail", Limit: 1200, Target: 1000, LastGood: 1200, State: Raising}, dayStats{Sent: 1000, Delivered: 1000}, enough, AtTarget, 1000, "at target"},
		{"under-used", p, raising(100, 1000), dayStats{Sent: 69, Delivered: 69}, enough, Holding, 100, "sent 69 of 100 yesterday, below 70%"},
		{"growth gmail", p, raising(100, 1000), dayStats{Sent: 70, Delivered: 70}, enough, Raising, 150, "clean day, limit x1.50"},
		{"growth default provider", p, Lane{Provider: "corp.example", Limit: 100, Target: 1000, LastGood: 100, State: Raising}, dayStats{Sent: 100, Delivered: 100}, enough, Raising, 140, "limit x1.40"},
		{"growth from sent above limit", p, Lane{Provider: "other", Limit: 100, Target: 1000, LastGood: 100, State: Raising}, dayStats{Sent: 120, Delivered: 120}, enough, Raising, 168, "limit x1.40"},
		{"growth capped at target", p, raising(100, 130), dayStats{Sent: 100, Delivered: 100}, enough, Raising, 130, "clean day"},
		{"growth on any day when grow_when_used is 0", p.With(func(p *Policy) { p.GrowWhenUsed = 0 }), raising(100, 1000), dayStats{Sent: 0}, dayStats{}, Raising, 150, "clean day"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lane := tc.lane
			v := tc.policy.verdict(&lane, tc.today, tc.win)
			if v.state != tc.state || v.limit != tc.limit {
				t.Errorf("verdict = %s %.0f (%q), want %s %.0f", v.state, v.limit, v.reason, tc.state, tc.limit)
			}
			if !strings.Contains(v.reason, tc.reason) {
				t.Errorf("reason %q lacks %q", v.reason, tc.reason)
			}
			if !reflect.DeepEqual(lane, tc.lane) {
				t.Errorf("verdict wrote to the lane: %+v", lane)
			}
		})
	}
}

func TestVerdictLastGood(t *testing.T) {
	p := DefaultPolicy()
	l := &Lane{Provider: "gmail", Limit: 100, Target: 1000, LastGood: 80, State: Raising}
	if v := p.verdict(l, dayStats{Sent: 100, Delivered: 100}, dayStats{Delivered: 100}); v.lastGood != 100 {
		t.Errorf("growth keeps lastGood %v, want the limit that grew (100)", v.lastGood)
	}
	if v := p.verdict(l, dayStats{Sent: 10, Delivered: 10}, dayStats{Delivered: 100}); v.lastGood != 80 {
		t.Errorf("hold lastGood = %v, want the previous 80", v.lastGood)
	}
	if v := p.verdict(l, dayStats{Complaints: 3}, dayStats{Delivered: 10, Complaints: 3}); v.lastGood != 100 {
		t.Errorf("pause lastGood = %v, want the limit before the pause (100)", v.lastGood)
	}
	l.Limit, l.Target = 1000, 1000
	if v := p.verdict(l, dayStats{Sent: 1000, Delivered: 1000}, dayStats{Delivered: 1000}); v.lastGood != 1000 {
		t.Errorf("at target lastGood = %v, want 1000", v.lastGood)
	}
}

func TestCloseDay(t *testing.T) {
	p := DefaultPolicy().With(func(p *Policy) { p.PauseDays = 2 })
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	l := &Lane{
		Source: "mx-1", Provider: "gmail", Limit: 100, Target: 1000, LastGood: 100, State: Raising, Day: 4,
		DayStart: start, HourStart: start, SentHour: 7, HourlyFactor: 0.5, BackedOff: false, Backoffs: 0,
		Sent: 80, Delivered: 78, Bounces: 1, Complaints: 0, IdleDays: 2,
		History: []dayStats{{Sent: 10, Delivered: 10}, {Sent: 20, Delivered: 20}, {Sent: 40, Delivered: 40}},
	}
	p.closeDay(l)
	if len(l.History) != p.TrailingDays {
		t.Fatalf("history = %d days, want %d", len(l.History), p.TrailingDays)
	}
	if want := (dayStats{Sent: 80, Delivered: 78, Bounces: 1}); l.History[2] != want || l.History[0].Sent != 20 {
		t.Errorf("history = %+v, want the oldest day dropped and today appended", l.History)
	}
	if l.State != Raising || l.Limit != 150 || l.LastGood != 100 || !strings.Contains(l.Reason, "clean day") {
		t.Errorf("after a clean day: state=%s limit=%v last_good=%v reason=%q", l.State, l.Limit, l.LastGood, l.Reason)
	}
	if l.Day != 5 || l.Sent != 0 || l.Delivered != 0 || l.Bounces != 0 || l.Complaints != 0 {
		t.Errorf("startDay did not reset the counters: %+v", l)
	}
	if l.HourlyFactor != 1 || l.BackedOff || l.Backoffs != 0 || !l.BlockedUntil.IsZero() {
		t.Errorf("startDay did not reset the intraday state: %+v", l)
	}
	if l.IdleDays != 0 {
		t.Errorf("idle days = %d after a day with traffic, want 0", l.IdleDays)
	}

	l.Complaints = 3
	p.closeDay(l)
	if l.State != Paused || l.Limit != 0 || l.PausedFor != 0 {
		t.Fatalf("after 3 complaints: state=%s limit=%v paused_for=%d, want paused 0 0", l.State, l.Limit, l.PausedFor)
	}
	if l.LastGood != 150 {
		t.Errorf("last good on pause = %v, want the limit before it (150)", l.LastGood)
	}
	p.closeDay(l)
	if l.State != Paused || l.PausedFor != 1 || l.IdleDays != 2 {
		t.Errorf("second pause day: state=%s paused_for=%d idle=%d, want paused 1 2", l.State, l.PausedFor, l.IdleDays)
	}
	p.closeDay(l)
	if l.State != Holding || l.Limit != 75 || l.PausedFor != 0 {
		t.Errorf("after the pause: state=%s limit=%v paused_for=%d, want holding 75 0", l.State, l.Limit, l.PausedFor)
	}
	if !strings.Contains(l.Reason, "resumed at 50%") {
		t.Errorf("reason = %q", l.Reason)
	}
}

func TestCloseDayIdleReset(t *testing.T) {
	p := DefaultPolicy().With(func(p *Policy) { p.IdleDaysReset = 2 })
	l := &Lane{Source: "mx-1", Provider: "gmail", Limit: 400, Target: 1000, LastGood: 400, State: Holding, HourlyFactor: 1}
	p.closeDay(l)
	if l.IdleDays != 1 || l.Limit != 400 {
		t.Fatalf("after one idle day: idle=%d limit=%v, want 1 and 400", l.IdleDays, l.Limit)
	}
	p.closeDay(l)
	if l.State != Raising || l.Limit != p.StartLimit || l.LastGood != p.StartLimit {
		t.Errorf("after the reset: state=%s limit=%v last_good=%v, want raising at start_limit", l.State, l.Limit, l.LastGood)
	}
	if l.IdleDays != 0 {
		t.Errorf("idle days = %d after the reset, want 0", l.IdleDays)
	}
}
