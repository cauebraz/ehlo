package kumomta

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/event"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

// documented is the Delivery record from the KumoMTA eval_template
// reference (docs/reference/string/eval_template.md at release
// 2026.09.29-b90d8bc1), with the addresses moved to 203.0.113.0/24 and the
// host names to example domains.
const documented = `{
    "type": "Delivery",
    "id": "1d98076abbbc11ed940250ebf67f93bd",
    "sender": "user@sender.example.com",
    "recipient": "user@gmail.com",
    "queue": "campaign:tenant@domain",
    "site": "source2->(alt1|alt2|alt3|alt4)?.mx.example.",
    "size": 1047,
    "response": {
        "code": 250,
        "enhanced_code": {"class": 2, "subject": 0, "detail": 0},
        "content": "OK ids=8a5475ccbbc611eda12250ebf67f93bd",
        "command": "."
    },
    "peer_address": {"name": "mx.example.", "addr": "203.0.113.27"},
    "timestamp": 1678069691,
    "created": 1678069691,
    "num_attempts": 0,
    "bounce_classification": "Uncategorized",
    "egress_pool": "pool0",
    "egress_source": "source2",
    "source_address": {"address": "203.0.113.1:53210", "protocol": "socks5", "server": "203.0.113.2:5000"},
    "feedback_report": null,
    "meta": {},
    "headers": {},
    "delivery_protocol": "ESMTP",
    "reception_protocol": "ESMTP",
    "nodeid": "557f3ad4-2c8c-11ee-976e-782d7e12e173",
    "tls_cipher": "TLS_AES_256_GCM_SHA384",
    "tls_protocol_version": "TLSv1.3",
    "tls_peer_subject_name": ["CN=mx.example"],
    "session_id": "9bcd689e-23d9-41b7-a015-63a1382f8b57"
}`

// withFields returns the documented record with the given fields replaced. A
// nil value removes the field.
func withFields(t *testing.T, over map[string]any) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(documented), &m); err != nil {
		t.Fatal(err)
	}
	maps.Copy(m, over)
	for k, v := range over {
		if v == nil {
			delete(m, k)
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func resp(code int, enhanced []int, content string, command any) map[string]any {
	r := map[string]any{"code": code, "content": content, "command": command}
	if enhanced != nil {
		r["enhanced_code"] = map[string]int{"class": enhanced[0], "subject": enhanced[1], "detail": enhanced[2]}
	}
	return r
}

func parse(t *testing.T, p Parser, body string) []event.Event {
	t.Helper()
	got, err := p.Parse([]byte(body), now)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	for _, e := range got {
		if err := e.Validate(); err != nil {
			t.Errorf("event %+v: %v", e, err)
		}
		if e.Route != "" {
			t.Errorf("route = %q, want empty (self_hosted)", e.Route)
		}
	}
	return got
}

func types(events []event.Event) string {
	var s []string
	for _, e := range events {
		s = append(s, string(e.Type))
	}
	return strings.Join(s, ",")
}

func TestDocumentedDelivery(t *testing.T) {
	got := parse(t, Parser{}, documented)
	if types(got) != "attempt,reply" {
		t.Fatalf("types = %s, want attempt,reply", types(got))
	}
	at, rp := got[0], got[1]
	want := time.Unix(1678069691, 0).UTC()
	for _, e := range got {
		if e.Source != "source2" || e.Domain != "gmail.com" || !e.Time.Equal(want) {
			t.Errorf("event = %+v, want source2, gmail.com, %v", e, want)
		}
	}
	if at.Attempt != event.First {
		t.Errorf("attempt = %q, want first for num_attempts 0", at.Attempt)
	}
	if rp.Code != 250 || rp.Enhanced != "2.0.0" || rp.Stage != event.StageData || rp.Text != "OK ids=8a5475ccbbc611eda12250ebf67f93bd" || rp.Delay != 0 {
		t.Errorf("reply = %+v", rp)
	}
}

func TestRecordTypes(t *testing.T) {
	cases := []struct {
		name  string
		over  map[string]any
		types string
		check func(t *testing.T, got []event.Event)
	}{
		{
			name:  "transient failure on retry keeps the provider reply",
			over:  map[string]any{"type": "TransientFailure", "num_attempts": 2, "response": resp(421, []int{4, 7, 0}, "[TSS04] Messages from 203.0.113.1 temporarily deferred", "RCPT TO:<user@yahoo.com>\r\n"), "recipient": "user@yahoo.com", "event_time": "2026-03-10T08:15:03.5Z", "created_time": "2026-03-10T08:00:00Z"},
			types: "attempt,reply",
			check: func(t *testing.T, got []event.Event) {
				at, rp := got[0], got[1]
				if at.Attempt != event.Retry {
					t.Errorf("attempt = %q, want retry", at.Attempt)
				}
				if rp.Code != 421 || rp.Enhanced != "4.7.0" || rp.Stage != event.StageRcpt || rp.Domain != "yahoo.com" || rp.Delay != 903.5 {
					t.Errorf("reply = %+v", rp)
				}
				if !rp.Time.Equal(time.Date(2026, 3, 10, 8, 15, 3, 500_000_000, time.UTC)) {
					t.Errorf("time = %v, want event_time", rp.Time)
				}
			},
		},
		{
			name:  "bounce at DATA with a policy class",
			over:  map[string]any{"type": "Bounce", "response": resp(550, []int{5, 7, 1}, "Message rejected as spam", "."), "bounce_classification": "SpamContent"},
			types: "attempt,reply,bounce",
			check: func(t *testing.T, got []event.Event) {
				if got[1].Code != 550 || got[1].Stage != event.StageData || got[2].Bounce != event.Policy {
					t.Errorf("events = %+v", got)
				}
			},
		},
		{
			name:  "invalid recipient bounce is hard",
			over:  map[string]any{"type": "Bounce", "response": resp(550, []int{5, 1, 1}, "No such user", "RCPT TO:<user@gmail.com>"), "bounce_classification": "InvalidRecipient"},
			types: "attempt,reply,bounce",
			check: func(t *testing.T, got []event.Event) {
				if got[2].Bounce != event.Hard {
					t.Errorf("bounce = %q, want hard", got[2].Bounce)
				}
			},
		},
		{
			name:  "uncategorized 5.7 bounce is policy",
			over:  map[string]any{"type": "Bounce", "response": resp(550, []int{5, 7, 26}, "Unauthenticated email is not accepted", "."), "bounce_classification": "Uncategorized"},
			types: "attempt,reply,bounce",
			check: func(t *testing.T, got []event.Event) {
				if got[2].Bounce != event.Policy {
					t.Errorf("bounce = %q, want policy", got[2].Bounce)
				}
			},
		},
		{
			name:  "greeting refusal is a connection failure with the provider code",
			over:  map[string]any{"type": "TransientFailure", "response": resp(421, nil, "Service not available", nil)},
			types: "attempt,reply",
			check: func(t *testing.T, got []event.Event) {
				if got[1].Stage != event.StageConnect || got[1].Code != 421 {
					t.Errorf("reply = %+v", got[1])
				}
			},
		},
		{
			name:  "failed to connect is a connection failure",
			over:  map[string]any{"type": "TransientFailure", "peer_address": nil, "tls_protocol_version": nil, "response": resp(400, nil, "KumoMTA internal: failed to connect to any candidate hosts: connect to 203.0.113.27:25: Connection refused", nil)},
			types: "attempt,reply",
			check: func(t *testing.T, got []event.Event) {
				if got[1].Stage != event.StageConnect || got[1].Code != 0 || got[1].Enhanced != "" {
					t.Errorf("reply = %+v", got[1])
				}
			},
		},
		{
			name:  "timeout after STARTTLS is a tls stage failure",
			over:  map[string]any{"type": "TransientFailure", "response": resp(421, []int{4, 4, 2}, "KumoMTA internal: failed to send message to mx.example: Timed Out waiting 60s for response to STARTTLS", "STARTTLS\r\n")},
			types: "attempt,reply",
			check: func(t *testing.T, got []event.Event) {
				if got[1].Stage != event.StageTLS {
					t.Errorf("stage = %q, want tls", got[1].Stage)
				}
			},
		},
		{
			name:  "timeout in DATA keeps the code",
			over:  map[string]any{"type": "TransientFailure", "response": resp(421, []int{4, 4, 2}, "KumoMTA internal: failed to send message to mx.example: Timed Out waiting 60s for response to DATA", "DATA\r\n")},
			types: "attempt,reply",
			check: func(t *testing.T, got []event.Event) {
				if got[1].Stage != event.StageData || got[1].Code != 421 || got[1].Enhanced != "4.4.2" {
					t.Errorf("reply = %+v", got[1])
				}
			},
		},
		{
			name:  "throttle inside KumoMTA is not an attempt",
			over:  map[string]any{"type": "TransientFailure", "peer_address": nil, "response": resp(451, []int{4, 4, 5}, "KumoMTA internal: source2 throttled message rate, delay=10s", nil)},
			types: "",
		},
		{
			name:  "suspended queue is not an attempt",
			over:  map[string]any{"type": "TransientFailure", "peer_address": nil, "response": resp(451, []int{4, 4, 4}, "KumoMTA internal: scheduled queue is suspended: maintenance", nil)},
			types: "",
		},
		{
			name:  "null MX bounce without a session is a bounce only",
			over:  map[string]any{"type": "Bounce", "peer_address": nil, "response": resp(556, []int{5, 1, 10}, "Recipient address has a null MX", nil), "bounce_classification": "BadDomain"},
			types: "bounce",
			check: func(t *testing.T, got []event.Event) {
				if got[0].Bounce != event.Hard {
					t.Errorf("bounce = %q, want hard", got[0].Bounce)
				}
			},
		},
		{
			name:  "MX without hosts is not an attempt",
			over:  map[string]any{"type": "TransientFailure", "peer_address": nil, "response": resp(451, []int{4, 4, 4}, "MX didn't resolve to any hosts", nil)},
			types: "",
		},
		{
			name:  "expiration is a soft bounce",
			over:  map[string]any{"type": "Expiration", "peer_address": nil, "delivery_protocol": nil, "response": resp(551, []int{5, 4, 7}, "Next delivery time would be at 2026-03-14 which exceeds the expiry time", nil)},
			types: "bounce",
			check: func(t *testing.T, got []event.Event) {
				if got[0].Bounce != event.Soft || got[0].Source != "source2" {
					t.Errorf("bounce = %+v, want soft from source2", got[0])
				}
			},
		},
		{
			name:  "expiration with only a pool is not counted under the pool",
			over:  map[string]any{"type": "Expiration", "peer_address": nil, "delivery_protocol": nil, "egress_source": nil, "response": resp(551, []int{5, 4, 7}, "Next delivery time would be at 2026-03-14 which exceeds the expiry time", nil)},
			types: "",
		},
		{
			name:  "out of band bounce names no egress source and is skipped",
			over:  map[string]any{"type": "OOB", "egress_pool": nil, "egress_source": nil, "delivery_protocol": nil, "response": resp(550, []int{5, 1, 1}, "user unknown", nil), "bounce_classification": "InvalidRecipient"},
			types: "",
		},
		{
			name: "abuse feedback takes the source from the trace header",
			over: map[string]any{"type": "Feedback", "recipient": "fbl@sender.example.com", "egress_pool": nil, "egress_source": nil, "delivery_protocol": nil, "feedback_report": map[string]any{
				"feedback_type": "abuse", "original_rcpto_to": []string{"<user@outlook.com>"}, "supplemental_trace": map[string]any{"recipient": "user@yahoo.com", "egress_source": "source1", "egress_pool": "pool0"},
			}},
			types: "complaint",
			check: func(t *testing.T, got []event.Event) {
				if got[0].Domain != "yahoo.com" || got[0].Source != "source1" {
					t.Errorf("complaint = %+v, want yahoo.com from source1 as the trace header names them", got[0])
				}
			},
		},
		{
			name: "abuse feedback without a traced source is skipped",
			over: map[string]any{"type": "Feedback", "recipient": "fbl@sender.example.com", "egress_pool": nil, "egress_source": nil, "delivery_protocol": nil, "feedback_report": map[string]any{
				"feedback_type": "abuse", "original_rcpto_to": []string{"<user@outlook.com>"}, "supplemental_trace": map[string]any{"recipient": "user@yahoo.com", "egress_pool": "pool0"},
			}},
			types: "",
		},
		{
			name: "abuse feedback with a non-string traced source is skipped",
			over: map[string]any{"type": "Feedback", "recipient": "fbl@sender.example.com", "egress_pool": nil, "egress_source": nil, "delivery_protocol": nil, "feedback_report": map[string]any{
				"feedback_type": "abuse", "supplemental_trace": map[string]any{"recipient": "user@yahoo.com", "egress_source": 7},
			}},
			types: "",
		},
		{
			name:  "not-spam feedback is skipped",
			over:  map[string]any{"type": "Feedback", "feedback_report": map[string]any{"feedback_type": "not-spam"}},
			types: "",
		},
		{
			name:  "reception is skipped",
			over:  map[string]any{"type": "Reception"},
			types: "",
		},
		{
			name:  "admin bounce is skipped",
			over:  map[string]any{"type": "AdminBounce"},
			types: "",
		},
		{
			name:  "lua delivery such as a webhook is skipped",
			over:  map[string]any{"delivery_protocol": "Lua"},
			types: "",
		},
		{
			name:  "unknown future type is skipped",
			over:  map[string]any{"type": "SomethingNew"},
			types: "",
		},
		{
			name:  "multi-recipient record counts per domain",
			over:  map[string]any{"recipient": []string{"a@gmail.com", "b@Gmail.com", "c@yahoo.com"}},
			types: "attempt,reply,attempt,reply",
			check: func(t *testing.T, got []event.Event) {
				if got[0].Domain != "gmail.com" || got[0].Count != 2 || got[1].Count != 2 || got[2].Domain != "yahoo.com" || got[2].N() != 1 {
					t.Errorf("events = %+v", got)
				}
			},
		},
		{
			name:  "missing timestamps fall back to now",
			over:  map[string]any{"timestamp": nil, "created": nil},
			types: "attempt,reply",
			check: func(t *testing.T, got []event.Event) {
				if !got[0].Time.Equal(now) {
					t.Errorf("time = %v, want now", got[0].Time)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parse(t, Parser{}, withFields(t, tc.over))
			if types(got) != tc.types {
				t.Fatalf("types = %q, want %q: %+v", types(got), tc.types, got)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

func TestSourceByPool(t *testing.T) {
	got := parse(t, Parser{By: ByPool}, documented)
	if got[0].Source != "pool0" {
		t.Errorf("source = %q, want pool0", got[0].Source)
	}
	if got := parse(t, Parser{By: ByPool}, withFields(t, map[string]any{"egress_pool": nil})); len(got) != 0 {
		t.Errorf("record without a pool = %+v, want no events", got)
	}
	expired := withFields(t, map[string]any{"type": "Expiration", "egress_source": nil, "peer_address": nil, "response": resp(551, []int{5, 4, 7}, "expired", nil)})
	if got := parse(t, Parser{By: ByPool}, expired); types(got) != "bounce" || got[0].Source != "pool0" {
		t.Errorf("expiration = %+v, want a bounce from pool0", got)
	}
	feedback := withFields(t, map[string]any{"type": "Feedback", "egress_pool": nil, "egress_source": nil, "feedback_report": map[string]any{
		"feedback_type": "abuse", "supplemental_trace": map[string]any{"recipient": "user@yahoo.com", "egress_source": "source1", "egress_pool": "pool1"},
	}})
	if got := parse(t, Parser{By: ByPool}, feedback); types(got) != "complaint" || got[0].Source != "pool1" {
		t.Errorf("complaint = %+v, want one from pool1", got)
	}
}

func TestParseBy(t *testing.T) {
	for in, want := range map[string]By{"": BySource, "egress_source": BySource, "egress_pool": ByPool} {
		if got, err := ParseBy(in); err != nil || got != want {
			t.Errorf("ParseBy(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	if _, err := ParseBy("pool"); err == nil {
		t.Error("ParseBy(pool) accepted")
	}
}

func TestBodyShapes(t *testing.T) {
	one := withFields(t, map[string]any{"type": "Delivery"})
	bodies := map[string]string{
		"object": one,
		"array":  "[" + one + "," + one + "]",
		"ndjson": one + "\n" + one + "\n",
	}
	wants := map[string]int{"object": 2, "array": 4, "ndjson": 4}
	for name, body := range bodies {
		if got := parse(t, Parser{}, body); len(got) != wants[name] {
			t.Errorf("%s: %d events, want %d", name, len(got), wants[name])
		}
	}
	for _, bad := range []string{"", "  ", "x", "[", `[{"type":"Delivery"}`, `[] []`, `"Delivery"`, `{"type":1}`} {
		if _, err := (Parser{}).Parse([]byte(bad), now); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if got, err := (Parser{}).Parse([]byte("[]"), now); err != nil || len(got) != 0 {
		t.Errorf("Parse([]) = %v, %v, want no events and no error", got, err)
	}
}

func TestBatchBound(t *testing.T) {
	rec := `{"type":"Reception"}`
	body := "[" + strings.Repeat(rec+",", event.MaxBatch) + rec + "]"
	if _, err := (Parser{}).Parse([]byte(body), now); !errors.Is(err, event.ErrBatchTooLarge) {
		t.Errorf("err = %v, want ErrBatchTooLarge", err)
	}
	many := make([]string, maxRecipients+1)
	for i := range many {
		many[i] = "u@d" + strings.Repeat("x", i%7) + ".example"
	}
	b, _ := json.Marshal(map[string]any{"type": "Delivery", "recipient": many, "egress_source": "s", "response": map[string]any{"code": 250}, "peer_address": map[string]any{"name": "mx"}})
	if _, err := (Parser{}).Parse(b, now); !errors.Is(err, event.ErrBatchTooLarge) {
		t.Errorf("err = %v, want ErrBatchTooLarge for a record with %d recipients", err, len(many))
	}
}

func TestLongTextIsTruncated(t *testing.T) {
	got := parse(t, Parser{}, withFields(t, map[string]any{"type": "TransientFailure", "response": resp(450, []int{4, 2, 0}, strings.Repeat("é", 400), "RCPT TO:<x>")}))
	if n := len(got[1].Text); n > event.MaxTextLength || n < event.MaxTextLength-1 {
		t.Errorf("text is %d bytes", n)
	}
}

func TestExampleRecords(t *testing.T) {
	b, err := os.ReadFile("../../../examples/kumomta/records.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	got := parse(t, Parser{}, string(b))
	if len(got) == 0 {
		t.Fatal("no events from the example records")
	}
}

// spread returns a record of the given type and response with n recipients,
// each at its own domain.
func spread(t *testing.T, typ string, n int, response map[string]any) string {
	t.Helper()
	rcpts := make([]string, n)
	for i := range rcpts {
		rcpts[i] = "u@" + strconv.FormatInt(int64(i), 36) + ".x"
	}
	b, err := json.Marshal(map[string]any{"type": typ, "recipient": rcpts, "egress_source": "s", "response": response, "peer_address": map[string]any{"name": "mx"}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// oneDomain returns a Bounce record with n recipients at one domain.
func oneDomain(t *testing.T, n int) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "Bounce", "recipient": slices.Repeat([]string{"u@x"}, n), "egress_source": "s", "response": resp(550, []int{5, 1, 1}, "No such user", "RCPT TO:<u@x>"), "peer_address": map[string]any{"name": "mx"}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fill repeats rec in a JSON array up to size bytes.
func fill(rec string, size int) string {
	n := max(1, (size-2)/(len(rec)+1))
	return "[" + strings.Repeat(rec+",", n-1) + rec + "]"
}

func TestEventBound(t *testing.T) {
	bounce := resp(550, []int{5, 1, 1}, "No such user", "RCPT TO:<u@x>")
	got, err := (Parser{}).Parse([]byte(spread(t, "Bounce", event.MaxBatch/3, bounce)), now)
	if err != nil || len(got) != event.MaxBatch/3*3 {
		t.Fatalf("Parse() = %d events, %v, want %d events", len(got), err, event.MaxBatch/3*3)
	}
	for name, body := range map[string]string{
		"one record":  spread(t, "Bounce", event.MaxBatch/3+1, bounce),
		"two records": "[" + spread(t, "Bounce", event.MaxBatch/5, bounce) + "," + spread(t, "Bounce", event.MaxBatch/5, bounce) + "]",
	} {
		if got, err := (Parser{}).Parse([]byte(body), now); !errors.Is(err, event.ErrBatchTooLarge) || got != nil {
			t.Errorf("%s: Parse() = %d events, %v, want ErrBatchTooLarge and no events", name, len(got), err)
		}
	}
}

func TestWorstCaseAllocationIsBounded(t *testing.T) {
	const size = 4 << 20
	const limit = 32 << 20
	cases := map[string]struct {
		body    string
		wantErr error
	}{
		"bounces to distinct domains": {fill(spread(t, "Bounce", maxRecipients, resp(550, []int{5, 1, 1}, "No such user", "RCPT TO:<u@x>")), size), event.ErrBatchTooLarge},
		"bounces to one domain":       {fill(oneDomain(t, maxRecipients), size), nil},
		"internal responses":          {fill(spread(t, "TransientFailure", maxRecipients, resp(451, []int{4, 4, 5}, "KumoMTA internal: s throttled message rate, delay=10s", nil)), size), nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			got, err := (Parser{}).Parse([]byte(tc.body), now)
			runtime.ReadMemStats(&after)
			if !errors.Is(err, tc.wantErr) || len(got) > event.MaxBatch {
				t.Errorf("Parse() = %d events, %v, want at most %d events and error %v", len(got), err, event.MaxBatch, tc.wantErr)
			}
			alloc := after.TotalAlloc - before.TotalAlloc
			t.Logf("body %d KiB, allocated %d KiB", len(tc.body)>>10, alloc>>10)
			if alloc > limit {
				t.Errorf("allocated %d MiB for a %d MiB body, want at most %d MiB", alloc>>20, size>>20, limit>>20)
			}
		})
	}
}
