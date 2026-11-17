// Package server is the ehlo HTTP surface: event ingest, ESP webhooks,
// the limits API and the Prometheus exposition.
package server

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/cauebraz/ehlo/internal/adapters/esp"
	"github.com/cauebraz/ehlo/internal/adapters/kumomta"
	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/ingest"
	"github.com/cauebraz/ehlo/internal/warmup"
)

// Defaults a Server starts with.
const (
	DefaultMaxBody    = 4 << 20 // matches config.DefaultMaxBodyBytes
	maxReportedErrors = 10
	maxErrorLength    = 256
)

// Server holds what the handlers share.
type Server struct {
	pipeline *ingest.Pipeline
	engine   *warmup.Engine
	gatherer prometheus.Gatherer
	maxBody  int64
	now      func() time.Time
	token    string
	sgKey    *ecdsa.PublicKey
}

// SendGrid signed webhook headers.
const (
	sendGridSignature = "X-Twilio-Email-Event-Webhook-Signature"
	sendGridTimestamp = "X-Twilio-Email-Event-Webhook-Timestamp"
)

// WithWebhookToken requires the token query parameter on every webhook.
func WithWebhookToken(token string) Option { return func(s *Server) { s.token = token } }

// WithSendGridKey requires a valid SendGrid signature on its webhook.
func WithSendGridKey(k *ecdsa.PublicKey) Option { return func(s *Server) { s.sgKey = k } }

// ParseSendGridKey decodes the base64 DER public key SendGrid shows in its
// Signed Event Webhook settings.
func ParseSendGridKey(b64 string) (*ecdsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("sendgrid public key: %w", err)
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("sendgrid public key: %w", err)
	}
	ec, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("sendgrid public key: not an ECDSA key")
	}
	return ec, nil
}

// Option configures a Server.
type Option func(*Server)

// WithMaxBody bounds one request body in bytes.
func WithMaxBody(n int64) Option { return func(s *Server) { s.maxBody = n } }

// WithClock replaces the wall clock.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

func utcNow() time.Time { return time.Now().UTC() }

// New wires the handlers to the pipeline that records events, the engine
// that answers limits and the registry that /metrics exposes.
func New(p *ingest.Pipeline, e *warmup.Engine, g prometheus.Gatherer, opts ...Option) *Server {
	s := &Server{pipeline: p, engine: e, gatherer: g, maxBody: DefaultMaxBody, now: utcNow}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Handler returns the routed HTTP handler. It rejects cross-origin browser
// requests with methods other than GET, HEAD and OPTIONS with 403, so a web
// page cannot post events through an operator's browser. Requests without
// Sec-Fetch-Site or Origin headers, as MTAs, scripts and ESPs send them,
// pass.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.gatherer, promhttp.HandlerOpts{}))
	mux.HandleFunc("POST /v1/events", s.events)
	mux.HandleFunc("POST /v1/webhooks/ses", s.webhook(esp.SES, s.snsSubscription))
	mux.HandleFunc("POST /v1/webhooks/sendgrid", s.webhook(esp.SendGrid, s.verifySendGrid))
	mux.HandleFunc("POST /v1/webhooks/kumomta", s.kumomta)
	mux.HandleFunc("GET /v1/limits", s.limits)
	mux.HandleFunc("GET /v1/limits/{source}/{provider}", s.limit)
	return http.NewCrossOriginProtection().Handler(mux)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

// IngestResult is the body of a successful POST /v1/events.
type IngestResult struct {
	Accepted int      `json:"accepted"`
	Invalid  int      `json:"invalid"`
	Errors   []string `json:"errors,omitempty"`
}

// events accepts one JSON object, a JSON array, or newline-delimited JSON.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	events, err := decodeEvents(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	s.record(w, events)
}

// precheck runs on a webhook body before parsing. It writes the response
// and returns true when the request is fully handled.
type precheck func(w http.ResponseWriter, r *http.Request, body []byte) bool

func (s *Server) webhook(parse esp.Parser, check precheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(s.token)) != 1 {
			http.Error(w, "invalid or missing token", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
		if err != nil {
			http.Error(w, err.Error(), statusFor(err))
			return
		}
		if check(w, r, body) {
			return
		}
		events, err := parse(body, s.now())
		if err == nil && len(events) > event.MaxBatch {
			err = event.ErrBatchTooLarge
		}
		if err != nil {
			http.Error(w, err.Error(), statusFor(err))
			return
		}
		s.record(w, events)
	}
}

// kumomta takes KumoMTA log records. The source query parameter picks the
// record field that names the source: egress_source (default) or
// egress_pool. Events keep the self_hosted route, because KumoMTA is the
// operator's own MTA and its lanes are the ones warmup manages.
func (s *Server) kumomta(w http.ResponseWriter, r *http.Request) {
	by, byErr := kumomta.ParseBy(r.URL.Query().Get("source"))
	check := func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		if byErr != nil {
			http.Error(w, byErr.Error(), http.StatusBadRequest)
			return true
		}
		return false
	}
	s.webhook(kumomta.Parser{By: by}.Parse, check)(w, r)
}

// snsSubscription logs an SNS SubscriptionConfirmation for an operator to
// confirm. ehlo makes no outbound calls, so it never visits the URL itself.
func (s *Server) snsSubscription(w http.ResponseWriter, _ *http.Request, body []byte) bool {
	sub, ok := esp.SNSSubscription(body)
	if !ok {
		return false
	}
	slog.Warn("SNS subscription waiting for confirmation: open subscribe_url or paste it in the SNS console", "topic", sub.TopicArn, "subscribe_url", sub.SubscribeURL)
	writeJSON(w, http.StatusOK, IngestResult{})
	return true
}

// verifySendGrid checks the ECDSA signature over timestamp and body when a
// public key is configured.
func (s *Server) verifySendGrid(w http.ResponseWriter, r *http.Request, body []byte) bool {
	if s.sgKey == nil {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(sendGridSignature))
	if err == nil {
		digest := sha256.Sum256(append([]byte(r.Header.Get(sendGridTimestamp)), body...))
		if ecdsa.VerifyASN1(s.sgKey, digest[:], sig) {
			return false
		}
	}
	http.Error(w, "invalid or missing SendGrid signature", http.StatusUnauthorized)
	return true
}

// statusFor maps a body read or decode error to 413 when the body hit the
// byte or event limit and 400 otherwise.
func statusFor(err error) int {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok || errors.Is(err, event.ErrBatchTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func (s *Server) record(w http.ResponseWriter, events []event.Event) {
	res := IngestResult{}
	for i := range events {
		if err := s.pipeline.Ingest(&events[i]); err != nil {
			res.Invalid++
			if len(res.Errors) < maxReportedErrors {
				res.Errors = append(res.Errors, truncate(fmt.Sprintf("event %d: %v", i, err), maxErrorLength))
			}
			continue
		}
		res.Accepted++
	}
	status := http.StatusAccepted
	if res.Accepted == 0 && res.Invalid > 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, res)
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

// decodeEvents reads one object, an array or NDJSON one event at a time,
// so memory follows the events kept, at most event.MaxBatch, and not the
// body. Any error means no event is returned and none is recorded.
func decodeEvents(r io.Reader) ([]event.Event, error) {
	br := bufio.NewReader(r)
	first, err := peekNonSpace(br)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty body")
		}
		return nil, err
	}
	if first != '[' && first != '{' {
		return nil, errors.New("body must be a JSON object, array or NDJSON")
	}
	dec := json.NewDecoder(br)
	if first == '[' {
		if _, err := dec.Token(); err != nil {
			return nil, fmt.Errorf("decode array: %w", err)
		}
	}
	var events []event.Event
	for first == '{' || dec.More() {
		var e event.Event
		err := dec.Decode(&e)
		if first == '{' && errors.Is(err, io.EOF) {
			break
		}
		if len(events) == event.MaxBatch {
			return nil, event.ErrBatchTooLarge
		}
		if err != nil {
			return nil, fmt.Errorf("decode event %d: %w", len(events), err)
		}
		events = append(events, e)
	}
	if first == '[' {
		if _, err := dec.Token(); err != nil {
			return nil, fmt.Errorf("decode array: %w", err)
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return nil, errors.New("decode array: data after the array")
		}
	}
	if len(events) == 0 {
		return nil, errors.New("no events")
	}
	return events, nil
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

// LimitsResult is the body of GET /v1/limits.
type LimitsResult struct {
	Lanes      []warmup.Decision `json:"lanes"`
	ServerTime time.Time         `json:"server_time"`
}

func (s *Server) limits(w http.ResponseWriter, r *http.Request) {
	lanes := s.engine.Lanes()
	if p := r.URL.Query().Get("provider"); p != "" {
		lanes = slices.DeleteFunc(lanes, func(d warmup.Decision) bool { return d.Provider != p })
	}
	if src := r.URL.Query().Get("source"); src != "" {
		lanes = slices.DeleteFunc(lanes, func(d warmup.Decision) bool { return d.Source != src })
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, LimitsResult{Lanes: lanes, ServerTime: s.now()})
}

func (s *Server) limit(w http.ResponseWriter, r *http.Request) {
	source, prov := r.PathValue("source"), r.PathValue("provider")
	if source == "" || prov == "" {
		http.Error(w, "source and provider are required", http.StatusBadRequest)
		return
	}
	d := s.engine.Decide(source, prov)
	w.Header().Set("Cache-Control", "no-cache")
	if !d.Managed {
		writeJSON(w, http.StatusNotFound, d)
		return
	}
	if d.ResumeAt != nil {
		if wait := d.ResumeAt.Sub(d.ServerTime); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		}
	}
	writeJSON(w, http.StatusOK, d)
}

// writeJSON encodes before writing the header, so an encode failure is a
// 500 and never a truncated 200.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
