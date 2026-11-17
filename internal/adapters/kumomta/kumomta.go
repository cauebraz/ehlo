// Package kumomta translates KumoMTA log records into events. The record
// shape is the JSON log record documented at
// https://docs.kumomta.com/reference/log_record/, as a log hook built with
// policy-extras.log_hooks posts it: one record per request from new_json,
// or an array from a batched hook.
package kumomta

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cauebraz/ehlo/internal/classify"
	"github.com/cauebraz/ehlo/internal/event"
)

// maxRecipients bounds the recipients one record may carry.
const maxRecipients = event.MaxBatch

// internalPrefix starts every response KumoMTA writes itself instead of
// relaying a reply from the receiving server.
const internalPrefix = "KumoMTA internal:"

// By selects the record field that becomes the event source. A record
// without that field gives no events, so a lane is never named after a
// field the delivery attempts do not use.
type By int

// Source fields.
const (
	// BySource uses egress_source.
	BySource By = iota
	// ByPool uses egress_pool.
	ByPool
)

func (b By) key() string {
	if b == ByPool {
		return "egress_pool"
	}
	return "egress_source"
}

// ParseBy reads the source query parameter: egress_source (or empty) or
// egress_pool.
func ParseBy(s string) (By, error) {
	switch s {
	case "", "egress_source":
		return BySource, nil
	case "egress_pool":
		return ByPool, nil
	}
	return BySource, fmt.Errorf("kumomta: source %q: want egress_source or egress_pool", s)
}

// Parser turns a request body of KumoMTA log records into events.
type Parser struct {
	By By
}

type enhancedCode struct {
	Class   int `json:"class"`
	Subject int `json:"subject"`
	Detail  int `json:"detail"`
}

type record struct {
	Type      string          `json:"type"`
	Recipient json.RawMessage `json:"recipient"`
	Response  struct {
		Code     int           `json:"code"`
		Enhanced *enhancedCode `json:"enhanced_code"`
		Content  string        `json:"content"`
		Command  *string       `json:"command"`
	} `json:"response"`
	Peer *struct {
		Name string `json:"name"`
	} `json:"peer_address"`
	Timestamp      float64 `json:"timestamp"`
	EventTime      string  `json:"event_time"`
	Created        float64 `json:"created"`
	CreatedTime    string  `json:"created_time"`
	NumAttempts    int     `json:"num_attempts"`
	Classification string  `json:"bounce_classification"`
	EgressPool     string  `json:"egress_pool"`
	EgressSource   string  `json:"egress_source"`
	Protocol       string  `json:"delivery_protocol"`
	Feedback       *struct {
		Type        string         `json:"feedback_type"`
		OriginalTo  []string       `json:"original_rcpto_to"`
		Supplements map[string]any `json:"supplemental_trace"`
	} `json:"feedback_report"`
}

// Parse accepts one record, a JSON array of records or one record per
// line, at most event.MaxBatch records that together give at most
// event.MaxBatch events. Delivery, TransientFailure and Bounce records of
// an SMTP delivery become an attempt and the reply, plus a bounce for
// Bounce. Expiration and OOB become a bounce, and Feedback of type abuse a
// complaint. Records without the source field, other record types,
// deliveries by another protocol than ESMTP, and failures KumoMTA raised
// before a session to the receiving server (throttles, suspended queues,
// unresolvable MX) return no events. A body that is not JSON, or over
// either limit, returns an error and no events.
func (p Parser) Parse(body []byte, now time.Time) ([]event.Event, error) {
	var out []event.Event
	var rcpts []string
	n := 0
	err := decode(body, func(r *record) error {
		var err error
		out, err = p.events(out, &rcpts, r, now)
		if err != nil {
			return fmt.Errorf("kumomta: record %d: %w", n, err)
		}
		n++
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// decode reads one object, an array or NDJSON one record at a time, hands
// each to fn, and stops past event.MaxBatch records.
func decode(body []byte, fn func(*record) error) error {
	br := bufio.NewReader(bytes.NewReader(body))
	first, err := peekNonSpace(br)
	if err != nil {
		return errors.New("kumomta: empty body")
	}
	if first != '[' && first != '{' {
		return errors.New("kumomta: body must be a JSON object, array or NDJSON")
	}
	dec := json.NewDecoder(br)
	if first == '[' {
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("kumomta: decode array: %w", err)
		}
	}
	for n := 0; first == '{' || dec.More(); n++ {
		var r record
		err := dec.Decode(&r)
		if first == '{' && errors.Is(err, io.EOF) {
			break
		}
		if n == event.MaxBatch {
			return fmt.Errorf("kumomta: %w", event.ErrBatchTooLarge)
		}
		if err != nil {
			return fmt.Errorf("kumomta: decode record %d: %w", n, err)
		}
		if err := fn(&r); err != nil {
			return err
		}
	}
	if first == '[' {
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("kumomta: decode array: %w", err)
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return errors.New("kumomta: decode array: data after the array")
		}
	}
	return nil
}

func peekNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if strings.IndexByte(" \t\r\n", b) < 0 {
			return b, br.UnreadByte()
		}
	}
}

// source reads the field p.By names. Feedback records carry no egress
// fields, so a Feedback record falls back on the same key in the decoded
// supplemental trace header, present when KumoMTA's trace_headers list it
// in include_meta_names.
func (p Parser) source(r *record) string {
	s := r.EgressSource
	if p.By == ByPool {
		s = r.EgressPool
	}
	if s == "" && r.Feedback != nil {
		s, _ = r.Feedback.Supplements[p.By.key()].(string)
	}
	return s
}

// events appends the events of one record to out and fails once out
// would hold more than event.MaxBatch events. rcpts is scratch space reused
// across records.
func (p Parser) events(out []event.Event, rcpts *[]string, r *record, now time.Time) ([]event.Event, error) {
	switch r.Type {
	case "Delivery", "TransientFailure", "Bounce":
		if r.Protocol != "" && r.Protocol != "ESMTP" {
			return out, nil
		}
	case "Expiration", "OOB":
	case "Feedback":
		if r.Feedback == nil || !strings.EqualFold(r.Feedback.Type, "abuse") {
			return out, nil
		}
		e := p.base(r, now)
		if e.Source == "" {
			return out, nil
		}
		e.Type, e.Domain = event.Complaint, feedbackDomain(r)
		return appendBounded(out, e)
	default:
		return out, nil
	}
	if p.source(r) == "" {
		return out, nil
	}
	tmpl := p.delivery(r, p.base(r, now))
	if len(tmpl) == 0 {
		return out, nil
	}
	domains, err := recipientDomains(r.Recipient, rcpts)
	if err != nil {
		return out, err
	}
	for _, d := range domains {
		for _, e := range tmpl {
			e.Domain = d.name
			if d.count > 1 {
				e.Count = d.count
			}
			if out, err = appendBounded(out, e); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

func appendBounded(out []event.Event, e event.Event) ([]event.Event, error) {
	if len(out) == event.MaxBatch {
		return out, event.ErrBatchTooLarge
	}
	return append(out, e), nil
}

func (p Parser) base(r *record, now time.Time) event.Event {
	return event.Event{Version: event.Version, Time: when(r.EventTime, r.Timestamp, now), Source: p.source(r)}
}

// delivery maps one record. The events carry no domain yet.
func (p Parser) delivery(r *record, base event.Event) []event.Event {
	if r.Type == "Expiration" {
		b := base
		b.Type, b.Bounce = event.Bounce, event.Soft
		return []event.Event{b}
	}
	reply := p.reply(r, base)
	if r.Type == "OOB" {
		b := base
		b.Type, b.Bounce = event.Bounce, bounceKind(r, &reply)
		return []event.Event{b}
	}
	internal := strings.HasPrefix(r.Response.Content, internalPrefix)
	connected := r.Peer != nil
	switch {
	case internal && strings.HasPrefix(r.Response.Content, internalPrefix+" failed to connect"):
		reply.Stage, reply.Code, reply.Enhanced = event.StageConnect, 0, ""
	case internal && strings.HasPrefix(r.Response.Content, internalPrefix+" failed to send message"):
	case internal, !connected:
		if r.Type == "Bounce" && !internal {
			b := base
			b.Type, b.Bounce = event.Bounce, bounceKind(r, &reply)
			return []event.Event{b}
		}
		return nil
	}
	attempt := base
	attempt.Type, attempt.Attempt = event.Attempt, event.First
	if r.NumAttempts > 0 {
		attempt.Attempt = event.Retry
	}
	if r.Type != "Bounce" {
		return []event.Event{attempt, reply}
	}
	b := base
	b.Type, b.Bounce = event.Bounce, bounceKind(r, &reply)
	return []event.Event{attempt, reply, b}
}

func (p Parser) reply(r *record, base event.Event) event.Event {
	e := base
	e.Type, e.Code = event.Reply, r.Response.Code
	if c := r.Response.Enhanced; c != nil && (c.Class == 2 || c.Class == 4 || c.Class == 5) && c.Subject >= 0 && c.Detail >= 0 && c.Subject < 1000 && c.Detail < 1000 {
		e.Enhanced = strconv.Itoa(c.Class) + "." + strconv.Itoa(c.Subject) + "." + strconv.Itoa(c.Detail)
	}
	e.Text = truncate(r.Response.Content, event.MaxTextLength)
	e.Tag = classify.Tag(e.Text)
	e.Stage = stageOf(r)
	if d := delay(r); d > 0 {
		e.Delay = d
	}
	if e.Stage.ConnectionLevel() {
		return e
	}
	if e.Code < 200 || e.Code > 599 || e.Code/100 == 3 {
		e.Stage, e.Code, e.Enhanced = event.StageConnect, 0, ""
	}
	return e
}

// stageOf reads the SMTP command the response answered. A response to no
// command is the greeting.
func stageOf(r *record) event.Stage {
	if r.Response.Command == nil {
		if r.Type == "Delivery" {
			return event.StageData
		}
		return event.StageConnect
	}
	c := strings.ToUpper(strings.TrimSpace(*r.Response.Command))
	switch {
	case strings.HasPrefix(c, "EHLO"), strings.HasPrefix(c, "HELO"), strings.HasPrefix(c, "LHLO"):
		return event.StageEHLO
	case strings.HasPrefix(c, "STARTTLS"):
		return event.StageTLS
	case strings.HasPrefix(c, "MAIL"):
		return event.StageMail
	case strings.HasPrefix(c, "RCPT"):
		return event.StageRcpt
	case strings.HasPrefix(c, "DATA"), strings.HasPrefix(c, "."), c == "":
		return event.StageData
	}
	return event.StageRcpt
}

// policyClasses are the KumoMTA bounce classifications where the receiver
// refused the sender or its content rather than the recipient.
var policyClasses = map[string]bool{
	"SpamBlock": true, "SpamContent": true, "SpamRelated": true, "PolicyRelated": true,
	"AuthenticationFailed": true, "ContentRelated": true, "VirusRelated": true, "ProhibitedAttachment": true,
}

// bounceKind maps the classification when it names a policy refusal or an
// expiry, and otherwise reads the reply as the Postfix adapter does: 5.7.x
// or a reputation, authentication or policy category is policy, 4xx soft,
// anything else hard.
func bounceKind(r *record, reply *event.Event) event.BounceKind {
	switch {
	case policyClasses[r.Classification]:
		return event.Policy
	case r.Classification == "MessageExpired":
		return event.Soft
	case strings.HasPrefix(reply.Enhanced, "5.7."):
		return event.Policy
	}
	switch classify.Event(reply) {
	case classify.ReputationBlock, classify.AuthFailure, classify.Policy:
		return event.Policy
	}
	if reply.Code/100 == 4 || strings.HasPrefix(reply.Enhanced, "4.") {
		return event.Soft
	}
	return event.Hard
}

type domainCount struct {
	name  string
	count int
}

// recipientDomains decodes recipient, a string or an array since KumoMTA
// 2025.12.02, into its domains with their recipient counts, in first-seen
// order. It decodes an array into scratch to reuse its backing array.
func recipientDomains(raw json.RawMessage, scratch *[]string) ([]domainCount, error) {
	var list []string
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, scratch); err != nil {
			return nil, fmt.Errorf("recipient: %w", err)
		}
		list = *scratch
	} else if len(raw) > 0 && string(raw) != "null" {
		var one string
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("recipient: %w", err)
		}
		list = []string{one}
	}
	if len(list) > maxRecipients {
		return nil, fmt.Errorf("%d recipients: %w", len(list), event.ErrBatchTooLarge)
	}
	if len(list) == 0 {
		return []domainCount{{}}, nil
	}
	var out []domainCount
	idx := map[string]int{}
	for _, rcpt := range list {
		d := domainOf(rcpt)
		if i, ok := idx[d]; ok {
			out[i].count++
			continue
		}
		idx[d] = len(out)
		out = append(out, domainCount{d, 1})
	}
	return out, nil
}

// feedbackDomain returns the domain of the complaining recipient: from
// KumoMTA's supplemental trace header when the report carried it, else
// from the report's Original-Rcpt-To.
func feedbackDomain(r *record) string {
	if s, ok := r.Feedback.Supplements["recipient"].(string); ok && domainOf(s) != "" {
		return domainOf(s)
	}
	for _, to := range r.Feedback.OriginalTo {
		if d := domainOf(to); d != "" {
			return d
		}
	}
	return ""
}

func domainOf(addr string) string {
	addr = strings.Trim(strings.TrimSpace(addr), "<>")
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

// when prefers the RFC 3339 event_time, then the unix timestamp, then now.
func when(rfc string, unix float64, now time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, rfc); err == nil {
		return t.UTC()
	}
	if unix > 0 && unix < 1e11 {
		return time.Unix(int64(unix), 0).UTC()
	}
	return now
}

// delay is the seconds from the message's creation to this record.
func delay(r *record) float64 {
	zero := time.Time{}
	at, created := when(r.EventTime, r.Timestamp, zero), when(r.CreatedTime, r.Created, zero)
	if at.IsZero() || created.IsZero() || at.Before(created) {
		return 0
	}
	return at.Sub(created).Seconds()
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
