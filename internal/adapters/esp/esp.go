// Package esp translates ESP webhooks into events, so an ESP route can sit
// next to self-hosted sources as a baseline.
package esp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cauebraz/ehlo/internal/event"
)

// Parser turns one webhook body into events. now is the time for events
// the payload does not date.
type Parser func(body []byte, now time.Time) ([]event.Event, error)

// SourceSES is the Source and Route of events parsed by SES.
const SourceSES = "ses"

// SourceSendGrid is the Source and Route of events parsed by SendGrid.
const SourceSendGrid = "sendgrid"

type sesEnvelope struct {
	Type         string `json:"Type"`
	Message      string `json:"Message"`
	SubscribeURL string `json:"SubscribeURL"`
	TopicArn     string `json:"TopicArn"`
}

// Subscription is an SNS SubscriptionConfirmation. ehlo makes no outbound
// calls, so an operator opens SubscribeURL to confirm it.
type Subscription struct {
	TopicArn     string
	SubscribeURL string
}

// SNSSubscription reports whether body is an SNS SubscriptionConfirmation
// and returns what an operator needs to confirm it.
func SNSSubscription(body []byte) (Subscription, bool) {
	var env sesEnvelope
	if json.Unmarshal(body, &env) != nil || env.Type != "SubscriptionConfirmation" {
		return Subscription{}, false
	}
	return Subscription{TopicArn: env.TopicArn, SubscribeURL: env.SubscribeURL}, true
}

// sesSuppressed lists the bounce subtypes where SES never attempted
// delivery. They say nothing about the provider and do not count toward
// the SES bounce rate.
var sesSuppressed = map[string]bool{
	"Suppressed":                true,
	"OnAccountSuppressionList":  true,
	"OnTenantSuppressionList":   true,
	"UnsubscribedRecipient":     true,
	"EmailValidationSuppressed": true,
}

type sesNotification struct {
	EventType        string `json:"eventType"`
	NotificationType string `json:"notificationType"`
	Mail             struct {
		Timestamp   time.Time `json:"timestamp"`
		Destination []string  `json:"destination"`
	} `json:"mail"`
	Bounce struct {
		BounceType        string    `json:"bounceType"`
		BounceSubType     string    `json:"bounceSubType"`
		Timestamp         time.Time `json:"timestamp"`
		BouncedRecipients []struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"bouncedRecipients"`
	} `json:"bounce"`
	Complaint struct {
		ComplaintSubType     string    `json:"complaintSubType"`
		Timestamp            time.Time `json:"timestamp"`
		ComplainedRecipients []struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"complainedRecipients"`
	} `json:"complaint"`
	Delivery struct {
		Timestamp  time.Time `json:"timestamp"`
		Recipients []string  `json:"recipients"`
	} `json:"delivery"`
	Open struct {
		Timestamp time.Time `json:"timestamp"`
	} `json:"open"`
}

// when returns the time of the event itself, falling back on the send
// time and then on now.
func (n *sesNotification) when(now time.Time) time.Time {
	for _, t := range []time.Time{n.Bounce.Timestamp, n.Complaint.Timestamp, n.Delivery.Timestamp, n.Open.Timestamp, n.Mail.Timestamp} {
		if !t.IsZero() {
			return t
		}
	}
	return now
}

// snsEnvelope unwraps an SNS envelope around an SES notification. It returns
// the inner message for a Notification, nil and true for a subscription
// message, and body unchanged when body is not an envelope, including when
// it is not JSON at all, so the decode error surfaces from the
// notification decoder with the right context.
func snsEnvelope(body []byte) (message []byte, control bool) {
	var env sesEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return body, false
	}
	switch {
	case env.Type == "SubscriptionConfirmation" || env.Type == "UnsubscribeConfirmation":
		return nil, true
	case env.Type == "Notification" && env.Message != "":
		return []byte(env.Message), false
	}
	return body, false
}

// SES accepts an SES event notification, directly or inside an SNS
// envelope. Delivery, Send, Bounce, Complaint and Open map to events, dated
// by the event's own timestamp. Bounces and complaints for addresses SES
// suppressed are skipped, because SES made no delivery attempt. SNS
// subscription messages and any other event type (Click, Reject,
// RenderingFailure, DeliveryDelay) return no events and no error. A body
// that is not JSON or a notification without a type returns an error.
func SES(body []byte, now time.Time) ([]event.Event, error) {
	body, control := snsEnvelope(body)
	if control {
		return nil, nil
	}
	var n sesNotification
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, fmt.Errorf("ses: decode notification: %w", err)
	}
	kind := strings.ToLower(n.EventType)
	if kind == "" {
		kind = strings.ToLower(n.NotificationType)
	}
	base := event.Event{Version: event.Version, Time: n.when(now), Source: SourceSES, Route: SourceSES}
	var out []event.Event
	switch kind {
	case "delivery", "send":
		for _, rcpt := range firstNonEmpty(n.Delivery.Recipients, n.Mail.Destination) {
			e := base
			e.Domain = domainOf(rcpt)
			if kind == "send" {
				e.Type = event.Attempt
			} else {
				e.Type, e.Code, e.Enhanced = event.Reply, 250, "2.0.0"
			}
			out = append(out, e)
		}
	case "bounce":
		if sesSuppressed[n.Bounce.BounceSubType] {
			return nil, nil
		}
		kind := event.Soft
		if strings.EqualFold(n.Bounce.BounceType, "Permanent") {
			kind = event.Hard
		}
		for _, r := range n.Bounce.BouncedRecipients {
			e := base
			e.Type, e.Bounce, e.Domain = event.Bounce, kind, domainOf(r.EmailAddress)
			out = append(out, e)
		}
	case "complaint":
		if sesSuppressed[n.Complaint.ComplaintSubType] {
			return nil, nil
		}
		for _, r := range n.Complaint.ComplainedRecipients {
			e := base
			e.Type, e.Domain = event.Complaint, domainOf(r.EmailAddress)
			out = append(out, e)
		}
	case "open":
		e := base
		e.Type = event.Open
		if len(n.Mail.Destination) == 1 {
			e.Domain = domainOf(n.Mail.Destination[0])
		}
		out = append(out, e)
	case "":
		return nil, errors.New("ses: notification without eventType")
	}
	return out, nil
}

type sendgridEvent struct {
	Email     string `json:"email"`
	Timestamp int64  `json:"timestamp"`
	Event     string `json:"event"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Response  string `json:"response"`
	Reason    string `json:"reason"`
}

var (
	smtpCode     = regexp.MustCompile(`^\s*([245]\d\d)\b`)
	enhancedCode = regexp.MustCompile(`\b([245]\.\d{1,3}\.\d{1,3})\b`)
)

// replyFrom fills code, enhanced status and text from a provider reply,
// keeping code and fallback when the text carries none.
func replyFrom(e *event.Event, text string, code int, fallback string) {
	e.Code, e.Enhanced = code, fallback
	if m := smtpCode.FindStringSubmatch(text); m != nil {
		e.Code, _ = strconv.Atoi(m[1])
	}
	if m := enhancedCode.FindStringSubmatch(text); m != nil {
		e.Enhanced = m[1]
	}
	if len(text) > event.MaxTextLength {
		text = text[:event.MaxTextLength]
	}
	e.Text = text
}

// SendGrid accepts the Event Webhook array. processed, delivered, bounce,
// deferred, spamreport and open map to events. A bounce of type blocked is
// soft, as SendGrid documents it. dropped is a SendGrid suppression with no
// delivery attempt, and it is skipped like click and other events.
func SendGrid(body []byte, now time.Time) ([]event.Event, error) {
	in, err := decodeSendGrid(body)
	if err != nil {
		return nil, err
	}
	var out []event.Event
	for _, s := range in {
		at := now
		if s.Timestamp > 0 {
			at = time.Unix(s.Timestamp, 0).UTC()
		}
		e := event.Event{Version: event.Version, Time: at, Source: SourceSendGrid, Route: SourceSendGrid, Domain: domainOf(s.Email)}
		switch s.Event {
		case "processed":
			e.Type = event.Attempt
		case "delivered":
			e.Type, e.Code, e.Enhanced = event.Reply, 250, "2.0.0"
		case "deferred":
			e.Type, e.Stage = event.Reply, event.StageRcpt
			replyFrom(&e, s.Response, 450, enhancedFromStatus(s.Status, "4.0.0"))
		case "bounce":
			e.Type, e.Bounce = event.Bounce, event.Hard
			if s.Type == "blocked" {
				e.Bounce = event.Soft
			}
		case "spamreport":
			e.Type = event.Complaint
		case "open":
			e.Type = event.Open
		default:
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// decodeSendGrid decodes the array one element at a time and stops past
// event.MaxBatch elements, so a body of empty objects cannot allocate a
// slice per byte.
func decodeSendGrid(body []byte) ([]sendgridEvent, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, errors.New("sendgrid: decode events: want a JSON array")
	}
	var in []sendgridEvent
	for dec.More() {
		if len(in) == event.MaxBatch {
			return nil, fmt.Errorf("sendgrid: %w", event.ErrBatchTooLarge)
		}
		var s sendgridEvent
		if err := dec.Decode(&s); err != nil {
			return nil, fmt.Errorf("sendgrid: decode event %d: %w", len(in), err)
		}
		in = append(in, s)
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("sendgrid: decode events: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("sendgrid: decode events: data after the array")
	}
	return in, nil
}

func enhancedFromStatus(status, fallback string) string {
	if status == "" {
		return fallback
	}
	return status
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

func firstNonEmpty(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}
