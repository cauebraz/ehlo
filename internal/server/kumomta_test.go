package server

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cauebraz/ehlo/internal/event"
	"github.com/cauebraz/ehlo/internal/warmup"
)

const kumoDelivery = `{"type":"Delivery","recipient":"user@gmail.com","response":{"code":250,"enhanced_code":{"class":2,"subject":0,"detail":0},"content":"OK","command":"."},"peer_address":{"name":"mx.example.","addr":"203.0.113.27"},"timestamp":1773129601,"created":1773129600,"num_attempts":0,"egress_pool":"pool0","egress_source":"ip-1","delivery_protocol":"ESMTP"}`

func TestKumoMTAWebhook(t *testing.T) {
	s, reg := newServer(t)
	h := s.Handler()
	body, err := os.ReadFile("../../examples/kumomta/records.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rec := postWith(h, "/v1/webhooks/kumomta", string(body), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s, want 202", rec.Code, rec.Body)
	}
	var res IngestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Invalid != 0 || res.Accepted == 0 {
		t.Errorf("result = %+v, %v", res, err)
	}
	if v := value(t, reg, "ehlo_smtp_attempts_total", "route", "self_hosted", "source", "ip-1", "provider", "gmail", "attempt", "first"); v != 1 {
		t.Errorf("gmail first attempts from ip-1 = %v, want 1", v)
	}
	if v := value(t, reg, "ehlo_smtp_attempts_total", "route", "self_hosted", "source", "ip-1", "provider", "yahoo", "attempt", "retry"); v != 1 {
		t.Errorf("yahoo retries from ip-1 = %v, want 1", v)
	}
	if v := value(t, reg, "ehlo_complaints_total", "route", "self_hosted", "source", "ip-1", "provider", "gmail"); v != 1 {
		t.Errorf("gmail complaints on ip-1 = %v, want 1 from the traced egress source", v)
	}
}

func TestKumoMTAUnattributedRecordsMakeNoLane(t *testing.T) {
	s, reg := newServerWith(t, warmup.Targets{Default: 1000})
	body := `{"type":"OOB","recipient":"user@gmail.com","response":{"code":550,"content":"user unknown"},"timestamp":1773129601}
{"type":"Feedback","recipient":"fbl@sender.example.com","feedback_report":{"feedback_type":"abuse","original_rcpto_to":["<user@gmail.com>"],"supplemental_trace":{"recipient":"user@gmail.com"}},"timestamp":1773129601}
`
	if rec := postWith(s.Handler(), "/v1/webhooks/kumomta", body, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s, want 202", rec.Code, rec.Body)
	}
	for _, typ := range []string{"bounce", "complaint"} {
		if v := value(t, reg, "ehlo_events_total", "type", typ, "result", "accepted"); v != 0 {
			t.Errorf("%s events = %v, want 0 for records without an egress source", typ, v)
		}
	}
	if lanes := s.engine.Lanes(); len(lanes) != 0 {
		t.Errorf("lanes = %+v, want none", lanes)
	}
}

func TestKumoMTASourceByPool(t *testing.T) {
	s, reg := newServer(t)
	rec := postWith(s.Handler(), "/v1/webhooks/kumomta?source=egress_pool", kumoDelivery, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	if v := value(t, reg, "ehlo_smtp_attempts_total", "route", "self_hosted", "source", "pool0", "provider", "gmail", "attempt", "first"); v != 1 {
		t.Errorf("attempts from pool0 = %v, want 1", v)
	}
	if rec := postWith(s.Handler(), "/v1/webhooks/kumomta?source=pool", kumoDelivery, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown source field = %d, want 400", rec.Code)
	}
}

func TestKumoMTAWebhookAuthAndBounds(t *testing.T) {
	h := newWebhookServer(t, WithWebhookToken("s3cret"), WithMaxBody(1<<10))
	for path, want := range map[string]int{
		"/v1/webhooks/kumomta":                              http.StatusUnauthorized,
		"/v1/webhooks/kumomta?token=wrong&source=nonsense":  http.StatusUnauthorized,
		"/v1/webhooks/kumomta?token=s3cret":                 http.StatusAccepted,
		"/v1/webhooks/kumomta?token=s3cret&source=nonsense": http.StatusBadRequest,
	} {
		if rec := postWith(h, path, kumoDelivery, nil); rec.Code != want {
			t.Errorf("POST %s = %d %s, want %d", path, rec.Code, rec.Body, want)
		}
	}
	big := "[" + strings.Repeat(kumoDelivery+",", 4) + kumoDelivery + "]"
	if rec := postWith(h, "/v1/webhooks/kumomta?token=s3cret", big, nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over the limit = %d, want 413", rec.Code)
	}
	if rec := postWith(h, "/v1/webhooks/kumomta?token=s3cret", "{", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("broken JSON = %d, want 400", rec.Code)
	}
	if rec := postWith(h, "/v1/webhooks/kumomta?token=s3cret", `{"type":"Reception"}`, nil); rec.Code != http.StatusAccepted {
		t.Errorf("record without events = %d %s, want 202 so KumoMTA does not fail it", rec.Code, rec.Body)
	}
}

func TestKumoMTAEventLimitRecordsNothing(t *testing.T) {
	s, reg := newServer(t)
	rcpts := make([]string, 4000)
	for i := range rcpts {
		rcpts[i] = "u@" + strconv.Itoa(i) + ".example"
	}
	rec, err := json.Marshal(map[string]any{"type": "Bounce", "recipient": rcpts, "egress_source": "ip-1", "response": map[string]any{"code": 550, "content": "No such user", "command": "RCPT TO:<u@x>"}, "peer_address": map[string]any{"name": "mx"}})
	if err != nil {
		t.Fatal(err)
	}
	if res := postWith(s.Handler(), "/v1/webhooks/kumomta", string(rec), nil); res.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("POST = %d %s, want 413 for more than %d events", res.Code, res.Body, event.MaxBatch)
	}
	if v := value(t, reg, "ehlo_events_total", "type", "bounce", "result", "accepted"); v != 0 {
		t.Errorf("bounces accepted = %v, want 0: a refused request records nothing", v)
	}
}
