// Package event defines the delivery events every adapter produces and the
// server consumes. The schema is versioned through Event.Version.
package event

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Version is the schema version this package reads and writes.
const Version = 1

// Type is what an event reports.
type Type string

// Event types. docs/events.md describes the fields each one carries.
const (
	Attempt   Type = "attempt"
	Reply     Type = "reply"
	Bounce    Type = "bounce"
	Complaint Type = "complaint"
	Open      Type = "open"
	Queue     Type = "queue"
	DMARC     Type = "dmarc"
	TLS       Type = "tls"
)

// Types lists every event type.
var Types = []Type{Attempt, Reply, Bounce, Complaint, Open, Queue, DMARC, TLS}

// Stage is where an SMTP session stopped. Everything before Mail is a
// connection-level failure, the only kind that lowers the Postfix window.
type Stage string

// Stages in the order a session goes through them.
const (
	StageConnect Stage = "connect"
	StageEHLO    Stage = "ehlo"
	StageTLS     Stage = "tls"
	StageMail    Stage = "mail"
	StageRcpt    Stage = "rcpt"
	StageData    Stage = "data"
)

// ConnectionLevel reports whether the session ended before MAIL FROM.
func (s Stage) ConnectionLevel() bool {
	return s == StageConnect || s == StageEHLO || s == StageTLS
}

// Attempted says whether a delivery attempt is the first for its message
// and recipient or a retry.
type Attempted string

// Attempt kinds.
const (
	First Attempted = "first"
	Retry Attempted = "retry"
)

// BounceKind separates hard, soft and policy bounces.
type BounceKind string

// Bounce kinds: hard for a permanent failure, soft for a transient one that
// expired, policy for a provider refusal.
const (
	Hard   BounceKind = "hard"
	Soft   BounceKind = "soft"
	Policy BounceKind = "policy"
)

// Result is an SPF or DKIM outcome in a DMARC row.
type Result string

// Authentication results.
const (
	Pass Result = "pass"
	Fail Result = "fail"
)

// Event is one delivery fact. Count batches identical facts.
type Event struct {
	Version int       `json:"v"`
	Type    Type      `json:"type"`
	Time    time.Time `json:"time"`

	// Source is the sending identity: a hostname, a pool member, an ESP.
	Source string `json:"source"`
	IP     string `json:"ip,omitempty"`
	// Route groups sources that are compared side by side, for example
	// self_hosted against an ESP baseline. Empty means self_hosted.
	Route string `json:"route,omitempty"`

	// Domain is the recipient domain. Provider overrides the classification
	// derived from it.
	Domain   string `json:"domain,omitempty"`
	Provider string `json:"provider,omitempty"`

	Count int `json:"count,omitempty"`

	// Attempt fields.
	Attempt Attempted `json:"attempt,omitempty"`
	Signed  *bool     `json:"signed,omitempty"`
	Engaged *bool     `json:"engaged,omitempty"`

	// Reply fields. Code is the three-digit SMTP code, Enhanced the
	// RFC 3463 status, Stage where the session stopped, Tag the provider's
	// own token ([TSS04], S3150), Text the reply text, Delay the seconds
	// from the message entering the queue to this reply.
	Code     int     `json:"code,omitempty"`
	Enhanced string  `json:"enhanced,omitempty"`
	Stage    Stage   `json:"stage,omitempty"`
	Tag      string  `json:"tag,omitempty"`
	Text     string  `json:"text,omitempty"`
	Delay    float64 `json:"delay,omitempty"`

	// Bounce fields.
	Bounce BounceKind `json:"bounce,omitempty"`

	// Queue fields: a snapshot of a source's deferred queue.
	Deferred  int           `json:"deferred,omitempty"`
	OldestAge time.Duration `json:"oldest_age,omitempty"`

	// DMARC fields: one row of an aggregate report.
	SPF  Result `json:"spf,omitempty"`
	DKIM Result `json:"dkim,omitempty"`

	// TLS fields: one established outbound session.
	TLSVersion string `json:"tls_version,omitempty"`
	TLSTrust   string `json:"tls_trust,omitempty"`
}

// Limits Validate enforces on one event.
const (
	MaxTextLength = 512
	MaxCount      = 10_000_000
)

// MaxBatch bounds the events one request may carry.
const MaxBatch = 10_000

// ErrBatchTooLarge reports a request with more than MaxBatch events.
var ErrBatchTooLarge = fmt.Errorf("more than %d events in one request", MaxBatch)

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid event")

// N is the number of facts the event batches, at least 1.
func (e *Event) N() int {
	if e.Count <= 0 {
		return 1
	}
	return e.Count
}

// Class groups replies by status class, plus connect for a session that
// ended before a transaction. It is the class label of the reply series.
type Class string

// Reply classes.
const (
	ClassConnect Class = "connect"
	Class2xx     Class = "2xx"
	Class4xx     Class = "4xx"
	Class5xx     Class = "5xx"
	ClassOther   Class = "other"
)

// RouteSelfHosted is the route of events that do not name one.
const RouteSelfHosted = "self_hosted"

// Class returns the reply's class.
func (e *Event) Class() Class {
	if e.Stage.ConnectionLevel() {
		return ClassConnect
	}
	switch {
	case e.Code >= 200 && e.Code < 300:
		return Class2xx
	case e.Code >= 400 && e.Code < 500:
		return Class4xx
	case e.Code >= 500 && e.Code < 600:
		return Class5xx
	}
	return ClassOther
}

// Delivered reports whether the reply is a 2xx.
func (e *Event) Delivered() bool { return e.Class() == Class2xx }

// CodeLabel is the bounded label for a reply: "250 2.0.0", or the class
// alone when the enhanced status is missing.
func (e *Event) CodeLabel() string {
	if e.Stage.ConnectionLevel() {
		return string(e.Stage)
	}
	if e.Enhanced == "" {
		return strconv.Itoa(e.Code)
	}
	return strconv.Itoa(e.Code) + " " + e.Enhanced
}

// Validate checks the fields the event type requires and the bounded
// values that become labels. Every error wraps ErrInvalid.
func (e *Event) Validate() error {
	if e.Version != 0 && e.Version != Version {
		return fmt.Errorf("%w: version %d, want %d", ErrInvalid, e.Version, Version)
	}
	if e.Source == "" {
		return fmt.Errorf("%w: source is required", ErrInvalid)
	}
	if !labelShape.MatchString(e.Source) {
		return fmt.Errorf("%w: source %q must be 1-64 letters, digits, '.', '_', ':' or '-'", ErrInvalid, e.Source)
	}
	if e.Route != "" && !labelShape.MatchString(e.Route) {
		return fmt.Errorf("%w: route %q must be 1-64 letters, digits, '.', '_', ':' or '-'", ErrInvalid, e.Route)
	}
	if e.Time.IsZero() {
		return fmt.Errorf("%w: time is required", ErrInvalid)
	}
	if e.Count < 0 || e.Count > MaxCount {
		return fmt.Errorf("%w: count %d outside 0-%d", ErrInvalid, e.Count, MaxCount)
	}
	switch e.Type {
	case Attempt:
		if e.Attempt != "" && e.Attempt != First && e.Attempt != Retry {
			return fmt.Errorf("%w: attempt %q", ErrInvalid, e.Attempt)
		}
	case Reply:
		if len(e.Text) > MaxTextLength {
			return fmt.Errorf("%w: reply text longer than %d bytes", ErrInvalid, MaxTextLength)
		}
		if e.Delay < 0 {
			return fmt.Errorf("%w: negative delay", ErrInvalid)
		}
		if e.Tag != "" && !ValidTag(e.Tag) {
			return fmt.Errorf("%w: tag %q", ErrInvalid, e.Tag)
		}
		if e.Stage.ConnectionLevel() {
			break
		}
		if e.Code < 200 || e.Code > 599 || e.Code/100 == 3 {
			return fmt.Errorf("%w: reply code %d", ErrInvalid, e.Code)
		}
		if e.Enhanced != "" && !validEnhanced(e.Enhanced) {
			return fmt.Errorf("%w: enhanced status %q", ErrInvalid, e.Enhanced)
		}
	case Bounce:
		if e.Bounce != Hard && e.Bounce != Soft && e.Bounce != Policy {
			return fmt.Errorf("%w: bounce kind %q", ErrInvalid, e.Bounce)
		}
	case Complaint, Open:
	case Queue:
		if e.Deferred < 0 || e.OldestAge < 0 {
			return fmt.Errorf("%w: negative queue values", ErrInvalid)
		}
	case DMARC:
		if (e.SPF != Pass && e.SPF != Fail) || (e.DKIM != Pass && e.DKIM != Fail) {
			return fmt.Errorf("%w: dmarc needs spf and dkim as pass or fail", ErrInvalid)
		}
	case TLS:
		if !validTLSVersion(e.TLSVersion) {
			return fmt.Errorf("%w: tls_version %q", ErrInvalid, e.TLSVersion)
		}
		if !validTLSTrust(e.TLSTrust) {
			return fmt.Errorf("%w: tls_trust %q", ErrInvalid, e.TLSTrust)
		}
	default:
		return fmt.Errorf("%w: type %q", ErrInvalid, e.Type)
	}
	return nil
}

func validEnhanced(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	if parts[0] != "2" && parts[0] != "4" && parts[0] != "5" {
		return false
	}
	for _, p := range parts[1:] {
		if p == "" || len(p) > 3 {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// labelShape bounds a source or route, the label values a client names.
var labelShape = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// tagShape bounds a provider tag: letters, digits and a few separators,
// at most 24 bytes. classify.Tag only produces tokens of this shape.
var tagShape = regexp.MustCompile(`^[A-Z0-9#:_()-]{2,24}$`)

// ValidTag reports whether s has the shape of a provider tag: 2-24 of
// A-Z, 0-9, #, :, _, (, ) and -.
func ValidTag(s string) bool { return tagShape.MatchString(s) }

// TLSVersions lists the accepted tls_version values, newest first.
func TLSVersions() []string { return []string{"TLSv1.3", "TLSv1.2", "TLSv1.1", "TLSv1", "none"} }

// TLSTrusts lists the accepted tls_trust values as Postfix logs them.
func TLSTrusts() []string { return []string{"verified", "trusted", "untrusted", "anonymous", "none"} }

func validTLSVersion(s string) bool { return slices.Contains(TLSVersions(), s) }

func validTLSTrust(s string) bool { return slices.Contains(TLSTrusts(), s) }

// CodeLabelWithTag is the code label with the provider tag appended, so
// Yahoo's 421 4.7.0 [TSS04] stays apart from a generic 421 4.7.0.
func (e *Event) CodeLabelWithTag() string {
	l := e.CodeLabel()
	if e.Tag != "" && !e.Stage.ConnectionLevel() {
		return l + " " + e.Tag
	}
	return l
}
