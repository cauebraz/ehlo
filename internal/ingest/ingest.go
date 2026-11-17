// Package ingest is the path every event takes: provider classification,
// tag extraction, validation, metrics and warmup.
package ingest

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/cauebraz/ehlo/internal/classify"
	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/metrics"
	"github.com/cauebraz/ehlo/internal/provider"
	"github.com/cauebraz/ehlo/internal/warmup"
)

// DefaultMaxSources bounds how many distinct sources the pipeline accepts.
const DefaultMaxSources = 256

// BuiltinRoutes are the routes every pipeline accepts.
var BuiltinRoutes = []string{event.RouteSelfHosted, "ses", "sendgrid"}

// ErrTooManySources rejects an event whose source would exceed the limit.
var ErrTooManySources = errors.New("too many sources")

// Pipeline enriches and records events. It is safe for concurrent use.
type Pipeline struct {
	providers  *provider.Classifier
	metrics    *metrics.Metrics
	engine     *warmup.Engine
	routes     []string
	maxSources int

	mu      sync.Mutex
	sources map[string]struct{}
}

// Option configures a Pipeline.
type Option func(*Pipeline)

// WithRoutes accepts routes beyond BuiltinRoutes.
func WithRoutes(routes []string) Option {
	return func(p *Pipeline) { p.routes = append(p.routes, routes...) }
}

// WithSources bounds the distinct sources to max and counts the given
// sources as already seen.
func WithSources(max int, seen []string) Option {
	return func(p *Pipeline) {
		p.maxSources = max
		for _, s := range seen {
			p.sources[s] = struct{}{}
		}
	}
}

// New wires the three stages together.
func New(providers *provider.Classifier, m *metrics.Metrics, e *warmup.Engine, opts ...Option) *Pipeline {
	p := &Pipeline{providers: providers, metrics: m, engine: e, routes: slices.Clone(BuiltinRoutes), maxSources: DefaultMaxSources, sources: map[string]struct{}{}}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Ingest classifies, validates and records one event. It returns the
// validation error, if any, after counting the event.
func (p *Pipeline) Ingest(e *event.Event) error {
	if e.Provider == "" {
		e.Provider = p.providers.Classify(e.Domain)
	} else if !p.providers.Known(e.Provider) {
		e.Provider = provider.Other
	}
	if e.Type == event.Reply && e.Tag == "" && e.Text != "" {
		e.Tag = classify.Tag(e.Text)
	}
	err := e.Validate()
	if err == nil {
		err = p.admit(e)
	}
	p.metrics.Count(e.Type, err == nil)
	if err != nil {
		return err
	}
	var cat classify.Category
	if e.Type == event.Reply {
		cat = classify.Event(e)
	}
	p.metrics.Observe(e, cat)
	p.engine.Observe(e, Signal(cat))
	return nil
}

// Signal maps a reply category to what the warmup policy reacts to.
func Signal(cat classify.Category) warmup.Signal {
	switch cat {
	case classify.RateLimited:
		return warmup.SignalThrottled
	case classify.ReputationBlock:
		return warmup.SignalBlocked
	}
	return warmup.SignalNone
}

// admit bounds the label values a client controls: the route must be
// configured and a new source must fit under the source limit.
func (p *Pipeline) admit(e *event.Event) error {
	if e.Route != "" && !slices.Contains(p.routes, e.Route) {
		return fmt.Errorf("%w: route %q is not configured", event.ErrInvalid, e.Route)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.sources[e.Source]; ok {
		return nil
	}
	if len(p.sources) >= p.maxSources {
		return fmt.Errorf("%w: %w: source %q would exceed max_sources %d", event.ErrInvalid, ErrTooManySources, e.Source, p.maxSources)
	}
	p.sources[e.Source] = struct{}{}
	return nil
}
