package warmup

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/cauebraz/ehlo/internal/event"
)

// Signal is what a reply tells the warmup policy: nothing, that the
// provider is rate limiting the lane, or that it blocked the source. The
// caller classifies the reply and maps the category to a signal.
type Signal int

// Signals in rising severity.
const (
	SignalNone Signal = iota
	SignalThrottled
	SignalBlocked
)

// Snapshot is what a Store persists.
type Snapshot struct {
	Version int     `json:"v"`
	Lanes   []*Lane `json:"lanes"`
}

// SnapshotVersion is the format of Snapshot. Restore rejects any other.
const SnapshotVersion = 2

// Engine owns the lanes. All methods are safe for concurrent use.
type Engine struct {
	now       func() time.Time
	policy    Policy
	targets   Targets
	providers map[string]struct{}
	known     map[string]struct{}

	mu       sync.Mutex
	lanes    map[laneKey]*Lane
	nextRoll time.Time
}

type laneKey struct{ source, provider string }

// Option configures an Engine.
type Option func(*Engine)

// WithClock replaces the wall clock. Tests and demos use it.
func WithClock(now func() time.Time) Option {
	return func(e *Engine) { e.now = now }
}

// WithKnownSources names sources the limits API may create lanes for before
// their first event. Sources in the targets are always known.
func WithKnownSources(sources []string) Option {
	return func(e *Engine) {
		for _, s := range sources {
			e.known[s] = struct{}{}
		}
	}
}

// New builds an engine. providers lists every provider the classifier
// returns; a lane is never created for another one.
func New(policy Policy, targets Targets, providers []string, opts ...Option) (*Engine, error) {
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("warmup policy: %w", err)
	}
	if err := targets.Validate(); err != nil {
		return nil, fmt.Errorf("warmup targets: %w", err)
	}
	e := &Engine{now: time.Now, policy: policy, targets: targets, providers: set(providers), known: map[string]struct{}{}, lanes: map[laneKey]*Lane{}}
	for _, o := range opts {
		o(e)
	}
	return e, nil
}

func set(names []string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// Policy returns the validated policy the engine runs.
func (e *Engine) Policy() Policy { return e.policy }

// managed reports whether events for the pair feed a lane.
func (e *Engine) managed(source, provider string) bool {
	_, ok := e.providers[provider]
	return ok && e.targets.daily(source) > 0
}

// Restore loads lanes from a snapshot. Targets are recomputed from the
// current config, and lanes for sources no longer managed are dropped. It
// returns how many were dropped.
func (e *Engine) Restore(s Snapshot) (int, error) {
	if s.Version != 0 && s.Version != SnapshotVersion {
		return 0, fmt.Errorf("snapshot version %d, want %d", s.Version, SnapshotVersion)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextRoll = time.Time{}
	dropped := 0
	for _, l := range s.Lanes {
		if l == nil || l.Source == "" || l.Provider == "" || !e.managed(l.Source, l.Provider) {
			dropped++
			continue
		}
		cp := *l
		if cp.DayStart.IsZero() {
			cp.DayStart = e.policy.DayStart(e.now())
		}
		cp.Target = e.targets.lane(cp.Source, cp.Provider, e.providers)
		cp.Limit = min(cp.Limit, cp.Target)
		if cp.HourlyFactor == 0 {
			cp.HourlyFactor = 1
		}
		e.lanes[laneKey{l.Source, l.Provider}] = &cp
	}
	return dropped, nil
}

// Snapshot copies every lane, sorted by source and provider.
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Snapshot{Version: SnapshotVersion, Lanes: make([]*Lane, 0, len(e.lanes))}
	for _, l := range e.lanes {
		cp := *l
		s.Lanes = append(s.Lanes, &cp)
	}
	slices.SortFunc(s.Lanes, compareLanes)
	return s
}

func (e *Engine) newLane(source, provider string, now time.Time) *Lane {
	target := e.targets.lane(source, provider, e.providers)
	l := &Lane{Source: source, Provider: provider, Target: target, DayStart: e.policy.DayStart(now), HourlyFactor: 1}
	if e.targets.warm(source) || target <= e.policy.StartLimit {
		l.Limit, l.State, l.Reason = target, AtTarget, "configured warm"
	} else {
		l.Limit, l.State, l.Reason = e.policy.StartLimit, Raising, "new lane"
	}
	l.LastGood = l.Limit
	e.lanes[laneKey{source, provider}] = l
	return l
}

// Observe feeds a valid event into its lane, with the signal its reply
// carries. Events for unmanaged sources, unknown providers or other routes
// are ignored. The lane rolls on the engine clock, never on the event's
// time, so a wrong clock on an adapter cannot move the policy day.
func (e *Engine) Observe(ev *event.Event, sig Signal) {
	if ev.Route != "" && ev.Route != event.RouteSelfHosted {
		return
	}
	if !e.managed(ev.Source, ev.Provider) {
		return
	}
	n := float64(ev.N())
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollLocked(now)
	l, ok := e.lanes[laneKey{ev.Source, ev.Provider}]
	if !ok {
		l = e.newLane(ev.Source, ev.Provider, now)
	}
	e.policy.rollHour(l, now)
	e.policy.rollWindow(l, now)
	switch ev.Type {
	case event.Attempt:
		if ev.Attempt != event.Retry {
			l.Sent += n
			l.SentHour += n
		}
		l.WindowAttempts += n
		e.policy.react(l, now)
	case event.Reply:
		if ev.Delivered() {
			l.Delivered += n
		}
		switch sig {
		case SignalThrottled:
			l.WindowThrottle += n
			e.policy.react(l, now)
		case SignalBlocked:
			l.WindowBlock += n
			e.policy.react(l, now)
		}
	case event.Bounce:
		l.Bounces += n
	case event.Complaint:
		l.Complaints += n
	}
}

// Decide answers a router's question for one lane. A lane is created on
// demand only for a known source and provider; any other pair is
// unmanaged, so a typo in a router never warms a ghost.
func (e *Engine) Decide(source, provider string) Decision {
	now := e.now()
	d := Decision{Source: source, Provider: provider, ServerTime: now}
	if !e.managed(source, provider) {
		return d
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollLocked(now)
	l, ok := e.lanes[laneKey{source, provider}]
	if !ok {
		if _, known := e.known[source]; !known && !e.targets.known(source) {
			return d
		}
		l = e.newLane(source, provider, now)
	}
	return e.policy.decision(l, now)
}

// Lanes returns every lane's decision, sorted by source and provider.
func (e *Engine) Lanes() []Decision {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollLocked(now)
	out := make([]Decision, 0, len(e.lanes))
	for _, l := range e.lanes {
		out = append(out, e.policy.decision(l, now))
	}
	slices.SortFunc(out, compareDecisions)
	return out
}

// Roll closes every policy day that ended before now.
func (e *Engine) Roll() {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollLocked(now)
}

func (e *Engine) rollLocked(now time.Time) {
	if now.Before(e.nextRoll) {
		return
	}
	today := e.policy.DayStart(now)
	for _, l := range e.lanes {
		for l.DayStart.Before(today) {
			e.policy.closeDay(l)
			l.DayStart = l.DayStart.Add(e.policy.DayLength)
		}
		if l.DayStart.After(today) {
			l.DayStart = today
		}
	}
	e.nextRoll = today.Add(e.policy.DayLength)
}
