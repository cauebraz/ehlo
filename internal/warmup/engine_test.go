package warmup

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/classify"
	"github.com/cauebraz/ehlo/internal/event"
)

var (
	providers = []string{"gmail", "outlook", "other"}
	day0      = time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
)

// clock drives the engine in tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func (c *clock) nextDay(n int)       { c.t = c.t.Add(time.Duration(n) * 24 * time.Hour) }

func newEngine(t *testing.T, targets Targets) (*Engine, *clock) {
	return newEngineWith(t, func(*Policy) {}, targets)
}

// newEngineWith builds an engine on the default policy changed by mod, on a
// test clock, with mx-1 as a known source.
func newEngineWith(t *testing.T, mod func(*Policy), targets Targets) (*Engine, *clock) {
	t.Helper()
	c := &clock{day0}
	e, err := New(DefaultPolicy().With(mod), targets, providers, WithClock(c.now), WithKnownSources([]string{"mx-1"}))
	if err != nil {
		t.Fatal(err)
	}
	return e, c
}

// feed classifies a reply the way the ingest pipeline does and observes it.
func feed(e *Engine, ev *event.Event) {
	sig := SignalNone
	if ev.Type == event.Reply {
		switch classify.Event(ev) {
		case classify.RateLimited:
			sig = SignalThrottled
		case classify.ReputationBlock:
			sig = SignalBlocked
		}
	}
	e.Observe(ev, sig)
}

func ev(t event.Type, src, prov string, n int) *event.Event {
	return &event.Event{Type: t, Time: day0, Source: src, Provider: prov, Count: n}
}

func attempt(src, prov string, n int) *event.Event { return ev(event.Attempt, src, prov, n) }

func complaint(n int) *event.Event { return ev(event.Complaint, "mx-1", "gmail", n) }

func reply(src, prov string, code int, n int) *event.Event {
	e := ev(event.Reply, src, prov, n)
	e.Code = code
	return e
}

func throttle(src, prov string, n int) *event.Event {
	e := reply(src, prov, 421, n)
	e.Enhanced = "4.7.28"
	return e
}

func block(src, prov string, n int) *event.Event {
	e := reply(src, prov, 550, n)
	e.Enhanced, e.Tag = "5.7.1", "S3150"
	return e
}

// cleanDay sends the whole limit and gets it all delivered.
func cleanDay(e *Engine, src, prov string) {
	d := e.Decide(src, prov)
	n := int(d.Limit)
	feed(e, attempt(src, prov, n))
	feed(e, reply(src, prov, 250, n))
}

func TestNewLaneStartsWarmingUp(t *testing.T) {
	e, _ := newEngine(t, Targets{Default: 100_000, Mix: map[string]float64{"gmail": 0.5}})
	d := e.Decide("mx-1", "gmail")
	if !d.Managed || d.Limit != 50 || d.State != Raising || !d.EngagedOnly || d.AllowedNow <= 0 {
		t.Errorf("new lane = %+v, want managed, limit 50, raising, engaged only, allowed_now > 0", d)
	}
	if d.Target != 50_000 {
		t.Errorf("gmail target = %v, want 50000", d.Target)
	}
	if o := e.Decide("mx-1", "outlook"); o.Target != 50_000 {
		t.Errorf("outlook target = %v, want 50000 (the whole unlisted remainder)", o.Target)
	}
	if d.DayStart != time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) || d.DayEnd != d.DayStart.Add(24*time.Hour) {
		t.Errorf("day = %v to %v", d.DayStart, d.DayEnd)
	}
	if d.ServerTime != day0 || d.Reason == "" {
		t.Errorf("server time %v, reason %q", d.ServerTime, d.Reason)
	}
}

func TestUnknownPairsDoNotCreateLanes(t *testing.T) {
	e, _ := newEngine(t, Targets{Default: 100_000})
	if d := e.Decide("typo-9", "gmail"); d.Managed {
		t.Errorf("unknown source got a lane: %+v", d)
	}
	if d := e.Decide("mx-1", "hotmail"); d.Managed {
		t.Errorf("unknown provider got a lane: %+v", d)
	}
	if n := len(e.Snapshot().Lanes); n != 0 {
		t.Errorf("lanes after unknown decisions = %d, want 0", n)
	}
	feed(e, attempt("typo-9", "gmail", 1))
	if d := e.Decide("typo-9", "gmail"); !d.Managed {
		t.Errorf("a source that sent events is managed: %+v", d)
	}
	feed(e, attempt("mx-1", "hotmail", 1))
	if n := len(e.Snapshot().Lanes); n != 1 {
		t.Errorf("unknown provider event created a lane, lanes = %d", n)
	}
}

func TestEventTimeDoesNotMoveTheDay(t *testing.T) {
	e, _ := newEngine(t, Targets{Default: 100_000})
	future := attempt("mx-1", "gmail", 10)
	future.Time = day0.AddDate(1, 0, 0)
	feed(e, future)
	d := e.Decide("mx-1", "gmail")
	if d.Day != 0 || d.Sent != 10 || d.DayStart.Year() != 2026 {
		t.Errorf("a future-dated event moved the lane: %+v", d)
	}
}

func TestWarmSourceStartsAtTarget(t *testing.T) {
	e, _ := newEngine(t, Targets{Default: 1000, Sources: map[string]SourceTarget{"warm-1": {Daily: 9000, Warm: true}}})
	d := e.Decide("warm-1", "gmail")
	if d.Limit != 9000 || d.State != AtTarget || d.EngagedOnly {
		t.Errorf("warm source = %+v, want limit 9000 (no mix, the whole target), at_target", d)
	}
}

func TestGrowthNeedsVolume(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 100_000})
	e.Decide("mx-1", "gmail")
	c.nextDay(1)
	d := e.Decide("mx-1", "gmail")
	if d.State != Holding || d.Limit != 50 {
		t.Errorf("idle day = %+v, want holding at 50", d)
	}
	feed(e, attempt("mx-1", "gmail", 20))
	feed(e, reply("mx-1", "gmail", 250, 20))
	c.nextDay(1)
	if d := e.Decide("mx-1", "gmail"); d.State != Holding || d.Limit != 50 {
		t.Errorf("20 of 50 sent = %+v, want holding", d)
	}
	feed(e, attempt("mx-1", "gmail", 40))
	feed(e, reply("mx-1", "gmail", 250, 40))
	c.nextDay(1)
	if d := e.Decide("mx-1", "gmail"); d.State != Raising || math.Abs(d.Limit-75) > 1e-9 {
		t.Errorf("40 of 50 sent = %+v, want raising to 75 (50 x 1.5)", d)
	}
}

func TestRampReachesTargetInAboutThreeWeeks(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 180_000, Mix: map[string]float64{"gmail": 0.55}})
	reached := -1
	for day := range 40 {
		if e.Decide("mx-1", "gmail").State == AtTarget {
			reached = day
			break
		}
		cleanDay(e, "mx-1", "gmail")
		c.nextDay(1)
	}
	if reached < 17 || reached > 23 {
		t.Errorf("gmail lane reached target on day %d, want 17 to 23 at x1.5 from 50 to 99000", reached)
	}
	if d := e.Decide("mx-1", "gmail"); d.EngagedOnly {
		t.Error("lane at target still asks for engaged only")
	}
}

func TestFirstDaysListenToComplaintCounts(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 100_000})
	feed(e, attempt("mx-1", "gmail", 40))
	feed(e, reply("mx-1", "gmail", 250, 40))
	feed(e, complaint(2))
	c.nextDay(1)
	if d := e.Decide("mx-1", "gmail"); d.State != Holding {
		t.Errorf("2 complaints on 40 deliveries = %s, want holding", d.State)
	}
	e2, c2 := newEngine(t, Targets{Default: 100_000})
	feed(e2, attempt("mx-1", "gmail", 40))
	feed(e2, reply("mx-1", "gmail", 250, 40))
	feed(e2, complaint(3))
	c2.nextDay(1)
	if d := e2.Decide("mx-1", "gmail"); d.State != Paused || d.AllowedNow != 0 || d.ResumeAt == nil {
		t.Errorf("3 complaints on 40 deliveries = %+v, want paused with resume_at", d)
	}
}

func TestTrailingWindowCatchesLateComplaints(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 1_000_000, Warm: []string{"mx-1"}})
	feed(e, attempt("mx-1", "gmail", 10_000))
	feed(e, reply("mx-1", "gmail", 250, 10_000))
	c.nextDay(1)
	feed(e, complaint(15))
	c.nextDay(1)
	if d := e.Decide("mx-1", "gmail"); d.State != Holding {
		t.Errorf("15 complaints a day after 10000 deliveries = %s, want holding (0.15%% over the window)", d.State)
	}
}

func TestPauseAndResumeAtHalf(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 1_000_000, Warm: []string{"mx-1"}})
	feed(e, attempt("mx-1", "gmail", 10_000))
	feed(e, reply("mx-1", "gmail", 250, 10_000))
	feed(e, complaint(40))
	c.nextDay(1)
	d := e.Decide("mx-1", "gmail")
	if d.State != Paused || d.Limit != 0 || d.AllowedNow != 0 {
		t.Fatalf("0.4%% complaints gave %+v, want paused", d)
	}
	c.nextDay(1)
	d = e.Decide("mx-1", "gmail")
	if d.State != Holding || math.Abs(d.Limit-d.Target/2) > 1e-6 {
		t.Errorf("after the pause = %+v, want holding at half the target", d)
	}
	if o := e.Decide("mx-1", "outlook"); o.State != AtTarget {
		t.Errorf("outlook lane of the same source = %s, want untouched", o.State)
	}
}

func TestResumedLaneDoesNotPauseOnTheOldDay(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 1_000_000, Warm: []string{"mx-1"}})
	feed(e, attempt("mx-1", "gmail", 10_000))
	feed(e, reply("mx-1", "gmail", 250, 10_000))
	feed(e, complaint(40))
	c.nextDay(2)
	if d := e.Decide("mx-1", "gmail"); d.State != Holding {
		t.Fatalf("after the pause = %s, want holding", d.State)
	}
	feed(e, attempt("mx-1", "gmail", 1_000))
	feed(e, reply("mx-1", "gmail", 250, 1_000))
	c.nextDay(1)
	d := e.Decide("mx-1", "gmail")
	if d.State == Paused || math.Abs(d.Limit-d.Target/2) > 1e-6 {
		t.Errorf("clean day after the resume = %+v, want the lane open at half the target", d)
	}
}

func TestBackoffHalvesTheHour(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 100_000})
	feed(e, attempt("mx-1", "gmail", 100))
	feed(e, reply("mx-1", "gmail", 250, 90))
	feed(e, throttle("mx-1", "gmail", 10))
	c.add(time.Second)
	d := e.Decide("mx-1", "gmail")
	if d.State != BackedOff || d.AllowedNow != 0 || d.ResumeAt == nil || !d.ResumeAt.Equal(day0.Add(10*time.Minute)) {
		t.Fatalf("after 10%% rate limited = %+v, want backoff for 10 minutes", d)
	}
	c.add(11 * time.Minute)
	d = e.Decide("mx-1", "gmail")
	if d.State != Raising || d.ResumeAt != nil {
		t.Errorf("after the backoff = %+v, want the lane open", d)
	}
	if want := 50.0 / 16 / 2; math.Abs(d.HourlyLimit-want) > 1e-9 {
		t.Errorf("hourly limit after one backoff = %v, want %v (halved)", d.HourlyLimit, want)
	}
	c.nextDay(1)
	if d := e.Decide("mx-1", "gmail"); d.State != Holding || d.HourlyLimit != 50.0/16 {
		t.Errorf("day after a backoff = %+v, want holding with the hour restored", d)
	}
}

func TestBlockEndsTheDay(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 1_000_000, Warm: []string{"mx-1"}})
	feed(e, attempt("mx-1", "outlook", 100))
	feed(e, reply("mx-1", "outlook", 250, 95))
	feed(e, block("mx-1", "outlook", 5))
	c.add(time.Second)
	d := e.Decide("mx-1", "outlook")
	if d.State != Blocked || d.AllowedNow != 0 || d.ResumeAt == nil || !d.ResumeAt.Equal(d.DayEnd) {
		t.Fatalf("5%% S3150 = %+v, want blocked until the next day", d)
	}
	c.add(2 * time.Hour)
	if d := e.Decide("mx-1", "outlook"); d.State != Blocked {
		t.Errorf("two hours later = %s, want still blocked", d.State)
	}
	c.nextDay(1)
	d = e.Decide("mx-1", "outlook")
	if d.State != Holding || math.Abs(d.Limit-d.Target/2) > 1e-6 {
		t.Errorf("day after a block = %+v, want holding at half the limit", d)
	}
}

func TestBackoffNeedsASample(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 100_000})
	feed(e, attempt("mx-1", "gmail", 5))
	feed(e, throttle("mx-1", "gmail", 5))
	c.add(time.Second)
	if d := e.Decide("mx-1", "gmail"); d.State == BackedOff {
		t.Error("5 attempts triggered a backoff, want min_attempts 20")
	}
}

func TestMailboxFullDoesNotBackOff(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 100_000})
	feed(e, attempt("mx-1", "gmail", 100))
	full := reply("mx-1", "gmail", 452, 50)
	full.Enhanced = "4.2.2"
	feed(e, full)
	c.add(time.Second)
	if d := e.Decide("mx-1", "gmail"); d.State == BackedOff {
		t.Error("mailbox-full replies triggered a backoff")
	}
}

func TestHourlyLimitOnlyWhileWarming(t *testing.T) {
	e, c := newEngineWith(t, func(p *Policy) { p.StartLimit = 1600 }, Targets{Default: 240_000, Warm: []string{"warm"}})
	c.t = time.Date(2026, 1, 5, 8, 30, 0, 0, time.UTC)
	d := e.Decide("mx-1", "other")
	if d.HourlyLimit != 100 || d.NextHourAt != time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC) {
		t.Errorf("warming lane hourly = %v next hour %v, want 1600/16 and 09:00", d.HourlyLimit, d.NextHourAt)
	}
	feed(e, attempt("mx-1", "other", 100))
	d = e.Decide("mx-1", "other")
	if d.AllowedNow >= 1 || d.Remaining <= 0 {
		t.Errorf("after an hour's worth = %+v, want allowed_now 0 and daily remaining > 0", d)
	}
	c.add(time.Hour)
	if d := e.Decide("mx-1", "other"); d.SentHour != 0 || d.AllowedNow != d.HourlyLimit {
		t.Errorf("next hour = %+v, want the hourly counter reset", d)
	}
	if w := e.Decide("warm", "other"); w.HourlyLimit != w.Limit {
		t.Errorf("lane at target hourly = %v, want the daily limit %v (no cap)", w.HourlyLimit, w.Limit)
	}
}

func TestBackoffCapsTheHourAtTarget(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 1000, Warm: []string{"mx-1"}})
	feed(e, attempt("mx-1", "other", 100))
	feed(e, reply("mx-1", "other", 250, 90))
	feed(e, throttle("mx-1", "other", 10))
	c.add(11 * time.Minute)
	d := e.Decide("mx-1", "other")
	if want := 1000.0 / 16 / 2; d.State != AtTarget || math.Abs(d.HourlyLimit-want) > 1e-9 {
		t.Errorf("lane at target after a backoff = %s hourly %v, want at_target with %v (limit / hours_per_day, halved)", d.State, d.HourlyLimit, want)
	}
}

func TestIdleReset(t *testing.T) {
	e, c := newEngineWith(t, func(p *Policy) { p.IdleDaysReset = 3 }, Targets{Default: 100_000, Warm: []string{"idle"}})
	e.Decide("idle", "gmail")
	c.nextDay(3)
	if d := e.Decide("idle", "gmail"); d.State != Raising || d.Limit != 50 {
		t.Errorf("after 3 idle days = %+v, want reset to start_limit", d)
	}
}

func TestShortDayLengthForDemos(t *testing.T) {
	e, c := newEngineWith(t, func(p *Policy) { p.DayLength = 24 * time.Minute }, Targets{Default: 100_000})
	d := e.Decide("mx-1", "gmail")
	if d.DayEnd.Sub(d.DayStart) != 24*time.Minute || d.NextHourAt.Sub(d.DayStart)%time.Minute != 0 {
		t.Errorf("short day = %v to %v, next hour %v", d.DayStart, d.DayEnd, d.NextHourAt)
	}
	cleanDay(e, "mx-1", "gmail")
	c.add(24 * time.Minute)
	if d := e.Decide("mx-1", "gmail"); d.Day != 1 || d.State != Raising {
		t.Errorf("after one short day = %+v, want day 1 raising", d)
	}
}

func TestSnapshotRoundTripRecomputesTargets(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "state", "warmup.json")}
	e, _ := newEngine(t, Targets{Default: 100_000})
	feed(e, attempt("mx-1", "gmail", 30))
	if err := store.Save(e.Snapshot()); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := newEngine(t, Targets{Default: 60_000})
	if _, err := e2.Restore(loaded); err != nil {
		t.Fatal(err)
	}
	d := e2.Decide("mx-1", "gmail")
	if d.Sent != 30 || d.Target != 60_000 {
		t.Errorf("restored = %+v, want sent 30 and the new target 60000", d)
	}
	if _, err := e2.Restore(Snapshot{Version: 99}); err == nil {
		t.Error("restore of an unknown version succeeded")
	}
}

func TestBaselineRoutesAreIgnored(t *testing.T) {
	e, _ := newEngine(t, Targets{Default: 100_000})
	feed(e, &event.Event{Type: event.Attempt, Time: day0, Source: "ses", Provider: "gmail", Route: "ses", Count: 500})
	if n := len(e.Snapshot().Lanes); n != 0 {
		t.Errorf("lanes for a baseline route = %d, want 0", n)
	}
}

func TestRetriesDoNotCountAgainstTheLimit(t *testing.T) {
	e, _ := newEngine(t, Targets{Default: 100_000})
	feed(e, attempt("mx-1", "gmail", 10))
	r := attempt("mx-1", "gmail", 7)
	r.Attempt = event.Retry
	feed(e, r)
	if d := e.Decide("mx-1", "gmail"); d.Sent != 10 || d.Remaining != 40 {
		t.Errorf("sent = %v remaining = %v, want 10 and 40", d.Sent, d.Remaining)
	}
}

func TestProviderWithZeroTargetIsUnmanaged(t *testing.T) {
	e, _ := newEngine(t, Targets{Default: 1000, Mix: map[string]float64{"gmail": 0.6, "outlook": 0.4}})
	if d := e.Decide("mx-1", "other"); d.Managed {
		t.Errorf("provider outside a mix that sums to 1 = %+v, want unmanaged", d)
	}
	if d := e.Decide("mx-1", "gmail"); !d.Managed || d.Target != 600 {
		t.Errorf("gmail lane = %+v, want managed with target 600", d)
	}
}

func TestPolicyValidation(t *testing.T) {
	bad := map[string]func(*Policy){
		"decision_hour":         func(p *Policy) { p.DecisionHour = 24 },
		"growth.gmail":          func(p *Policy) { p.Growth["gmail"] = 1 },
		"growth.default":        func(p *Policy) { delete(p.Growth, DefaultKey) },
		"pause_complaint_ratio": func(p *Policy) { p.HoldComplaintRatio, p.PauseComplaintRatio = 0.005, 0.004 },
		"pause_complaints":      func(p *Policy) { p.HoldComplaints, p.PauseComplaints = 3, 2 },
		"day_length":            func(p *Policy) { p.DayLength = time.Second },
		"backoff.ratio":         func(p *Policy) { p.Backoff.Ratio = 2 },
		"shaping.default":       func(p *Policy) { delete(p.Shaping, DefaultKey) },
		"hold_complaint_ratio":  func(p *Policy) { p.HoldComplaintRatio, p.PauseComplaintRatio = 2, 3 },
		"hold_bounce_ratio":     func(p *Policy) { p.HoldBounceRatio = 5 },
	}
	for field, mod := range bad {
		_, err := New(DefaultPolicy().With(mod), Targets{}, providers)
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want it named", field, err)
		}
	}
	if _, err := New(Policy{}, Targets{}, providers); err == nil {
		t.Error("a zero policy was accepted")
	}
	zero := DefaultPolicy().With(func(p *Policy) { p.IdleDaysReset, p.GrowWhenUsed = 0, 0 })
	if err := zero.Validate(); err != nil {
		t.Errorf("idle_days_reset 0 and grow_when_used 0 rejected: %v", err)
	}
}

func TestRestoreAlignsDayStartToTheCurrentPolicy(t *testing.T) {
	e, c := newEngineWith(t, func(p *Policy) { p.DecisionHour = 6 }, Targets{Default: 100_000})
	midnight := time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
	_, err := e.Restore(Snapshot{Version: SnapshotVersion, Lanes: []*Lane{
		{Source: "mx-1", Provider: "gmail", Limit: 200, DayStart: midnight, Sent: 10},
		{Source: "mx-1", Provider: "outlook", Limit: 200},
	}})
	if err != nil {
		t.Fatal(err)
	}
	today := time.Date(2026, 1, 5, 6, 0, 0, 0, time.UTC)
	for _, prov := range []string{"gmail", "outlook"} {
		d := e.Decide("mx-1", prov)
		if !d.DayStart.Equal(today) || !d.DayEnd.Equal(today.Add(24*time.Hour)) {
			t.Errorf("%s day = %v to %v, want %v to %v", prov, d.DayStart, d.DayEnd, today, today.Add(24*time.Hour))
		}
		if d.Day > 2 {
			t.Errorf("%s day counter = %d after a zero or stale day start", prov, d.Day)
		}
	}
	c.add(time.Hour)
	if d := e.Decide("mx-1", "gmail"); d.Sent != 0 {
		t.Errorf("stale day's sent carried over: %v", d.Sent)
	}
}

func TestHourSlotsFollowDecisionHour(t *testing.T) {
	e, c := newEngineWith(t, func(p *Policy) { p.DecisionHour = 7; p.StartLimit = 1600 }, Targets{Default: 240_000})
	c.t = time.Date(2026, 1, 5, 6, 45, 0, 0, time.UTC)
	feed(e, attempt("mx-1", "other", 50))
	c.t = time.Date(2026, 1, 5, 7, 5, 0, 0, time.UTC)
	d := e.Decide("mx-1", "other")
	if d.SentHour != 0 || !d.NextHourAt.Equal(time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("first slot of the day = sent_hour %v next %v, want 0 and 08:00", d.SentHour, d.NextHourAt)
	}
}

func TestResumeNeverExceedsTarget(t *testing.T) {
	e, c := newEngine(t, Targets{Default: 100_000, Sources: map[string]SourceTarget{"small": {Daily: 20}}})
	feed(e, attempt("small", "outlook", 100))
	feed(e, block("small", "outlook", 5))
	c.nextDay(1)
	if d := e.Decide("small", "outlook"); d.Limit > d.Target {
		t.Errorf("after a block = limit %v target %v, want limit <= target", d.Limit, d.Target)
	}
}
