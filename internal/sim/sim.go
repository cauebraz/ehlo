// Package sim is a load generator that behaves like a sending platform:
// it asks the ehlo server for limits, sends accordingly, and reports the
// delivery events a real MTA and feedback loop would produce.
package sim

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/warmup"
)

// Scenario selects the failure mode the generated traffic exhibits.
type Scenario string

// The scenarios the simulator can play.
const (
	Steady         Scenario = "steady"
	Warmup         Scenario = "warmup"
	OverCapacity   Scenario = "over-capacity"
	GmailThrottle  Scenario = "gmail-throttle"
	ConnectRefused Scenario = "connect-refused"
	DKIMDown       Scenario = "dkim-down"
	ComplaintSpike Scenario = "complaint-spike"
	UnknownSource  Scenario = "unknown-source"
)

// Scenarios lists every Scenario in the order the command line shows them.
var Scenarios = []Scenario{Steady, Warmup, OverCapacity, GmailThrottle, ConnectRefused, DKIMDown, ComplaintSpike, UnknownSource}

// ScenarioNames returns the scenario names as strings, in Scenarios order.
func ScenarioNames() []string {
	names := make([]string, len(Scenarios))
	for i, sc := range Scenarios {
		names[i] = string(sc)
	}
	return names
}

// ParseScenario converts a command-line name into a Scenario.
func ParseScenario(s string) (Scenario, error) {
	if slices.Contains(Scenarios, Scenario(s)) {
		return Scenario(s), nil
	}
	return "", fmt.Errorf("unknown scenario %q, want one of %s", s, strings.Join(ScenarioNames(), ", "))
}

// Limits is what the simulator needs from the server.
type Limits interface {
	Decide(ctx context.Context, source, provider string) (warmup.Decision, error)
}

// Config describes the platform the simulator imitates.
type Config struct {
	Scenario Scenario
	// Sources are the warm sending identities. NewSource, when set, is a
	// source the server has never seen, added by the warmup scenario.
	Sources   []string
	NewSource string
	// DailyVolume is the self-hosted volume the platform tries to send per
	// day, spread over the diurnal curve.
	DailyVolume float64
	// Baselines are ESP routes that receive BaselineShare of the volume.
	Baselines     []string
	BaselineShare float64
	// QueueRunDelay and QueueLifetime model the MTA's deferred queue.
	QueueRunDelay time.Duration
	QueueLifetime time.Duration
	// DayLength is the simulated day the end-of-day rollup follows, matching
	// the server's -day-length. Zero means 24h.
	DayLength time.Duration
	Start     time.Time
	Seed      uint64
}

// Provider is one modeled destination: its share of the mix, its feedback
// rates and the replies its MX returns.
type Provider struct {
	Name          string
	Domain        string
	Mix           float64
	ComplaintRate float64
	OpenRate      float64
	Replies       []Reply
}

// Reply is one SMTP response a provider returns at Rate per attempt.
type Reply struct {
	Code     int
	Enhanced string
	Stage    event.Stage
	Rate     float64
	Text     string
	Tag      string
}

const (
	codeDelivered  = 250
	codeRateLimit  = 421
	enhRateLimit   = "4.7.28"
	enhConnect     = "4.4.1"
	enhMailboxFull = "4.2.2"
)

func replies(connect, busy, full, rateLimit, unknown, policy float64) []Reply {
	return []Reply{
		{0, enhConnect, event.StageConnect, connect, "connect to mx: Connection timed out", ""},
		{450, "4.2.1", event.StageRcpt, busy, "The user you are trying to contact is receiving mail at a rate that prevents additional messages from being delivered", ""},
		{452, enhMailboxFull, event.StageRcpt, full, "The email account that you tried to reach is over quota", ""},
		{codeRateLimit, enhRateLimit, event.StageRcpt, rateLimit, "Our system has detected an unusual rate of unsolicited mail originating from your IP address", ""},
		{550, "5.1.1", event.StageRcpt, unknown, "The email account that you tried to reach does not exist", ""},
		{550, "5.7.1", event.StageMail, policy, "The user or domain that you are sending to (or from) has a policy that prohibited the mail that you sent", ""},
	}
}

var yahooThrottle = Reply{421, "4.7.0", event.StageMail, 0, "[TSS04] Messages from 203.0.113.10 temporarily deferred due to unexpected volume or user complaints", "TSS04"}
var outlookThrottle = Reply{451, "4.7.650", event.StageMail, 0, "The mail server [203.0.113.10] has been temporarily rate limited due to IP reputation. (S775)", "S775"}

// throttle is how a provider rate-limits a source in the GmailThrottle
// scenario. A zero reply keeps the provider's own rate-limit reply.
type throttle struct {
	reply Reply
	rate  float64
}

var throttles = map[string]throttle{
	"gmail":   {rate: throttledRateLimit},
	"yahoo":   {reply: yahooThrottle, rate: throttledRateLimit / 3},
	"outlook": {reply: outlookThrottle, rate: throttledRateLimit / 3},
}

// Providers is the modeled destination mix. The rates are plausible, not
// measured.
var Providers = []Provider{
	{"gmail", "gmail.com", 0.55, 0.0005, 0.22, replies(0.001, 0.003, 0.003, 0.002, 0.008, 0.0015)},
	{"outlook", "outlook.com", 0.20, 0.0006, 0.18, replies(0.001, 0.004, 0.002, 0, 0.009, 0.002)},
	{"yahoo", "yahoo.com", 0.10, 0.0007, 0.17, replies(0.001, 0.003, 0.002, 0, 0.010, 0.002)},
	{"other", "example.net", 0.15, 0.0003, 0.15, replies(0.002, 0.004, 0.003, 0, 0.012, 0.001)},
}

const (
	baselineHardBounce = 0.008
	baselineSoftBounce = 0.004
	retrySuccess       = 0.7
	throttledRetry     = 0.3
	throttledRateLimit = 0.30
	refusedConnectRate = 0.60
	spikeComplaintRate = 0.004
	overCapacityFactor = 2.0
	perConnection      = 50
	unknownSourceDaily = 2000
	throttledSources   = 2
	dkimDownSource     = 2
	defaultDayLength   = 24 * time.Hour
)

var secondsPerDay = defaultDayLength.Seconds()

// unlimited stands in for a decision when the server does not manage the
// lane or cannot be reached.
var unlimited = warmup.Decision{Limit: math.Inf(1), AllowedNow: math.Inf(1)}

type cohort struct {
	provider int
	reply    Reply
	enqueued time.Time
	n        float64
}

type source struct {
	name     string
	deferred []cohort
	nextScan time.Time
	signed   float64
	unsigned float64
}

// carry accumulates the fraction of n that rounding would drop, per key,
// so a rate of 0.3 per step still produces an event every few steps
// instead of never.
type carry[K comparable] map[K]float64

func (c carry[K]) whole(k K, n float64) int {
	n += c[k]
	w := math.Round(n)
	c[k] = n - w
	return int(w)
}

type laneKey struct{ source, provider int }

type identityKey struct {
	typ      event.Type
	source   string
	domain   string
	enhanced string
	stage    event.Stage
	bounce   event.BounceKind
	spf      event.Result
	dkim     event.Result
	tls      string
}

func identity(e *event.Event) identityKey {
	return identityKey{e.Type, e.Source, e.Domain, e.Enhanced, e.Stage, e.Bounce, e.SPF, e.DKIM, e.TLSVersion}
}

// Sim holds the generator state between steps.
type Sim struct {
	ctx     context.Context
	cfg     Config
	limits  Limits
	rng     *rand.Rand
	clock   time.Time
	sources []*source
	out     []event.Event
	errs    []error
	daily   map[string]float64
	lanes   carry[laneKey]
	frac    carry[identityKey]
	// wall stamps events with the real time, so the server's lag histogram
	// reads the tail delay and not the simulated date.
	wall func() time.Time
}

// seedMix spreads one seed across the two PCG streams.
const seedMix = 0x9e3779b97f4a7c15

// New builds a simulator. It makes no request until the first Step.
func New(cfg Config, limits Limits) *Sim {
	if cfg.DayLength <= 0 {
		cfg.DayLength = defaultDayLength
	}
	s := &Sim{
		cfg:    cfg,
		limits: limits,
		rng:    rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^seedMix)),
		clock:  cfg.Start,
		daily:  map[string]float64{},
		lanes:  carry[laneKey]{},
		frac:   carry[identityKey]{},
		wall:   time.Now,
	}
	names := append([]string(nil), cfg.Sources...)
	if cfg.NewSource != "" {
		names = append(names, cfg.NewSource)
	}
	for i, n := range names {
		s.sources = append(s.sources, &source{name: n, nextScan: cfg.Start.Add(time.Duration(i) * cfg.QueueRunDelay / time.Duration(max(1, len(names))))})
	}
	return s
}

// Clock is the simulated time.
func (s *Sim) Clock() time.Time { return s.clock }

// Step advances the simulated clock by dt and returns the events the
// platform produced, plus any errors talking to the limits API. ctx bounds
// the limits queries the step makes.
func (s *Sim) Step(ctx context.Context, dt time.Duration) ([]event.Event, []error) {
	s.ctx = ctx
	defer func() { s.ctx = nil }()
	s.out, s.errs = s.out[:0], s.errs[:0]
	for dt > 0 {
		next := s.clock.Truncate(s.cfg.DayLength).Add(s.cfg.DayLength)
		chunk := min(dt, next.Sub(s.clock))
		s.send(chunk)
		s.clock = s.clock.Add(chunk)
		s.scan()
		dt -= chunk
		if !s.clock.Before(next) {
			s.endOfDay()
		}
	}
	for _, src := range s.sources {
		var queued float64
		var oldest time.Duration
		for _, c := range src.deferred {
			queued += c.n
			oldest = max(oldest, s.clock.Sub(c.enqueued))
		}
		s.emit(event.Event{Type: event.Queue, Source: src.name, Deferred: count(queued), OldestAge: oldest, Count: 1})
	}
	return slices.Clone(s.out), slices.Clone(s.errs)
}

func diurnal(t time.Time) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60
	return 1 + 0.6*math.Sin((h-9)/24*2*math.Pi)
}

func (s *Sim) emit(e event.Event) {
	if e.Count <= 0 {
		return
	}
	e.Version = event.Version
	e.Time = s.wall().UTC()
	s.out = append(s.out, e)
}

func count(n float64) int { return int(math.Round(n)) }

func (s *Sim) send(dt time.Duration) {
	volume := s.cfg.DailyVolume * diurnal(s.clock) * dt.Seconds() / secondsPerDay * max(0.5, 1+0.05*s.rng.NormFloat64())
	if s.cfg.Scenario == OverCapacity {
		volume *= overCapacityFactor
	}
	baseline := volume * s.cfg.BaselineShare
	for _, route := range s.cfg.Baselines {
		for p := range Providers {
			s.sendBaseline(route, p, baseline/float64(len(s.cfg.Baselines))*Providers[p].Mix)
		}
	}
	self := volume - baseline
	for p := range Providers {
		s.route(p, self*Providers[p].Mix)
	}
}

func (s *Sim) sendBaseline(route string, p int, n float64) {
	prov := Providers[p]
	base := event.Event{Source: route, Route: route, Domain: prov.Domain}
	s.daily[route] += n
	s.emitKind(base, event.Attempt, n)
	hard, soft := n*baselineHardBounce, n*baselineSoftBounce
	s.emitAccepted(base, prov, n-hard-soft, prov.ComplaintRate)
	b := base
	b.Type, b.Bounce = event.Bounce, event.Hard
	s.emitN(b, hard)
	b.Bounce = event.Soft
	s.emitN(b, soft)
}

// emitAccepted reports n messages the provider took: the 250 reply, then
// the complaints and opens that follow delivery.
func (s *Sim) emitAccepted(base event.Event, prov Provider, n, complaintRate float64) {
	if n <= 0 {
		return
	}
	r := base
	r.Type, r.Code, r.Enhanced, r.Stage = event.Reply, codeDelivered, "2.0.0", event.StageData
	r.Text, r.Delay = "OK", s.delay(Reply{Code: codeDelivered})
	s.emitN(r, n)
	s.emitKind(base, event.Complaint, n*complaintRate)
	s.emitKind(base, event.Open, n*prov.OpenRate)
}

func (s *Sim) emitKind(base event.Event, t event.Type, n float64) {
	base.Type = t
	s.emitN(base, n)
}

// emitN emits e with a count carried across steps.
func (s *Sim) emitN(e event.Event, n float64) {
	if n <= 0 {
		return
	}
	e.Count = s.frac.whole(identity(&e), n)
	s.emit(e)
}

// route splits a provider's volume across sources by the server's limits:
// each source takes its share of the summed limits, capped by what it has
// left today, and the rest goes to sources with room.
func (s *Sim) route(p int, amount float64) {
	if amount <= 0 {
		return
	}
	decisions, total := s.collectDecisions(Providers[p].Name)
	placed, sum := initialShares(amount, decisions, total)
	spillOverflow(placed, decisions, amount-sum)
	for i, n := range placed {
		if math.IsNaN(n) || n <= 0 {
			continue
		}
		if whole := s.lanes.whole(laneKey{i, p}, n); whole > 0 {
			s.deliver(i, p, float64(whole), decisions[i].EngagedOnly)
		}
	}
}

// collectDecisions asks the server about every source and returns the
// decisions with the sum of the finite limits.
func (s *Sim) collectDecisions(provider string) ([]warmup.Decision, float64) {
	decisions := make([]warmup.Decision, len(s.sources))
	var total float64
	for i, src := range s.sources {
		d, err := s.limits.Decide(s.ctx, src.name, provider)
		switch {
		case err != nil:
			s.errs = append(s.errs, err)
			d = unlimited
		case !d.Managed:
			d.Limit, d.AllowedNow = unlimited.Limit, unlimited.AllowedNow
		}
		decisions[i] = d
		if !math.IsInf(d.Limit, 1) {
			total += d.Limit
		}
	}
	return decisions, total
}

// initialShares gives each source its share of amount, proportional to its
// limit when the server set one, capped by what it may send now.
func initialShares(amount float64, decisions []warmup.Decision, total float64) ([]float64, float64) {
	placed := make([]float64, len(decisions))
	var sum float64
	for i, d := range decisions {
		share := amount / float64(len(decisions))
		if total > 0 && !math.IsInf(d.Limit, 1) {
			share = amount * d.Limit / total
		}
		placed[i] = min(share, d.AllowedNow)
		sum += placed[i]
	}
	return placed, sum
}

// spillOverflow spreads what the capped sources could not take over the
// sources with room left, in proportion to that room.
func spillOverflow(placed []float64, decisions []warmup.Decision, overflow float64) {
	if overflow <= 0 {
		return
	}
	var room float64
	for i, d := range decisions {
		room += max(0, d.AllowedNow-placed[i])
	}
	for i, d := range decisions {
		if r := max(0, d.AllowedNow-placed[i]); r > 0 && room > 0 {
			placed[i] += min(overflow, room) * r / room
		}
	}
}

func (s *Sim) deliver(i, p int, n float64, engagedOnly bool) {
	src := s.sources[i]
	prov := Providers[p]
	signed := s.cfg.Scenario != DKIMDown || i != dkimDownSource%len(s.sources)
	engaged := engagedOnly
	base := event.Event{Source: src.name, Domain: prov.Domain}
	a := base
	a.Type, a.Attempt, a.Signed, a.Engaged, a.Count = event.Attempt, event.First, &signed, &engaged, count(n)
	s.emit(a)
	if signed {
		src.signed += n
	} else {
		src.unsigned += n
	}
	var rejected, deferred float64
	for _, r := range prov.Replies {
		r, rate := s.replyRate(i, prov, r)
		k := n * rate
		if k <= 0 {
			continue
		}
		s.emitReply(base, r, k)
		switch {
		case r.Stage.ConnectionLevel() || r.Code/100 == 4:
			deferred += k
			src.deferred = append(src.deferred, cohort{provider: p, reply: r, enqueued: s.clock, n: k})
		default:
			rejected += k
			kind := event.Hard
			if strings.HasPrefix(r.Enhanced, "5.7.") {
				kind = event.Policy
			}
			b := base
			b.Type, b.Bounce = event.Bounce, kind
			s.emitN(b, k)
		}
	}
	s.accepted(i, p, n-rejected-deferred)
}

// replyRate returns the reply the provider gives source i for r and the
// rate at which it gives it, after the scenario's adjustments.
func (s *Sim) replyRate(i int, prov Provider, r Reply) (Reply, float64) {
	switch {
	case r.Enhanced == enhRateLimit && s.cfg.Scenario == GmailThrottle && i >= len(s.sources)-throttledSources:
		if t, ok := throttles[prov.Name]; ok {
			if t.reply.Code != 0 {
				r = t.reply
			}
			return r, t.rate
		}
	case r.Stage == event.StageConnect && prov.Name == "outlook" && s.cfg.Scenario == ConnectRefused:
		return r, refusedConnectRate
	}
	return r, r.Rate
}

func (s *Sim) emitReply(base event.Event, r Reply, n float64) {
	e := base
	e.Type, e.Code, e.Enhanced, e.Stage = event.Reply, r.Code, r.Enhanced, r.Stage
	e.Text, e.Tag = r.Text, r.Tag
	e.Delay = s.delay(r)
	if r.Stage.ConnectionLevel() {
		e.Code, e.Enhanced = 0, ""
	}
	s.emitN(e, n)
}

// delay models seconds from queue entry to the reply: most mail leaves in
// under two seconds, deferred retries carry the queue wait.
func (s *Sim) delay(r Reply) float64 {
	base := 0.3 + 1.5*s.rng.Float64()
	if r.Code/100 == 4 || r.Stage.ConnectionLevel() {
		base += 30 * s.rng.Float64()
	}
	return base
}

func (s *Sim) accepted(i, p int, n float64) {
	if n <= 0 {
		return
	}
	prov := Providers[p]
	base := event.Event{Source: s.sources[i].name, Domain: prov.Domain}
	rate := prov.ComplaintRate
	if s.cfg.Scenario == ComplaintSpike && prov.Name == "gmail" && i == len(s.sources)-1 {
		rate = spikeComplaintRate
	}
	s.emitAccepted(base, prov, n, rate)
	tls := base
	tls.Type, tls.TLSVersion, tls.TLSTrust = event.TLS, "TLSv1.3", "untrusted"
	s.emitN(tls, n/float64(perConnection))
}

func (s *Sim) scan() {
	for i, src := range s.sources {
		for !src.nextScan.After(s.clock) {
			s.runQueue(i)
			src.nextScan = src.nextScan.Add(s.cfg.QueueRunDelay)
		}
	}
}

func (s *Sim) runQueue(i int) {
	src := s.sources[i]
	kept := src.deferred[:0]
	for _, c := range src.deferred {
		prov := Providers[c.provider]
		base := event.Event{Source: src.name, Domain: prov.Domain}
		if s.clock.Sub(c.enqueued) >= s.cfg.QueueLifetime {
			b := base
			b.Type, b.Bounce = event.Bounce, event.Soft
			s.emitN(b, c.n)
			continue
		}
		a := base
		a.Type, a.Attempt = event.Attempt, event.Retry
		s.emitN(a, c.n)
		success := retrySuccess
		switch {
		case c.reply.Enhanced == enhMailboxFull:
			success = 0
		case c.reply.Enhanced == enhRateLimit && s.cfg.Scenario == GmailThrottle:
			success = throttledRetry
		case c.reply.Stage == event.StageConnect && prov.Name == "outlook" && s.cfg.Scenario == ConnectRefused:
			success = 1 - refusedConnectRate
		}
		ok := c.n * success
		s.accepted(i, c.provider, ok)
		c.n -= ok
		if c.n >= 0.5 || success == 0 {
			s.emitReply(base, c.reply, c.n)
			kept = append(kept, c)
		}
	}
	src.deferred = kept
}

func (s *Sim) endOfDay() {
	for _, src := range s.sources {
		s.emit(event.Event{Type: event.DMARC, Source: src.name, SPF: event.Pass, DKIM: event.Pass, Count: count(src.signed)})
		s.emit(event.Event{Type: event.DMARC, Source: src.name, SPF: event.Pass, DKIM: event.Fail, Count: count(src.unsigned)})
		src.signed, src.unsigned = 0, 0
	}
	for _, route := range s.cfg.Baselines {
		s.emit(event.Event{Type: event.DMARC, Source: route, Route: route, SPF: event.Pass, DKIM: event.Pass, Count: count(s.daily[route])})
		s.daily[route] = 0
	}
	if s.cfg.Scenario == UnknownSource {
		s.emit(event.Event{Type: event.DMARC, Source: "unknown", SPF: event.Fail, DKIM: event.Fail, Count: unknownSourceDaily})
	}
}
