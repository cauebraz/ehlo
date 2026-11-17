package esp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/event"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func TestSESBounceInsideSNS(t *testing.T) {
	msg := `{"eventType":"Bounce","bounce":{"bounceType":"Permanent","bouncedRecipients":[{"emailAddress":"user4@gmail.com"}]},"mail":{"timestamp":"2026-03-10T08:00:00.000Z","destination":["user4@gmail.com"]}}`
	env, _ := json.Marshal(map[string]string{"Type": "Notification", "Message": msg})
	got, err := SES(env, now)
	if err != nil || len(got) != 1 {
		t.Fatalf("SES() = %+v, %v", got, err)
	}
	e := got[0]
	if e.Type != event.Bounce || e.Bounce != event.Hard || e.Domain != "gmail.com" || e.Route != SourceSES || e.Source != SourceSES {
		t.Errorf("event = %+v", e)
	}
	if !e.Time.Equal(time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("time = %v", e.Time)
	}
	if err := e.Validate(); err != nil {
		t.Error(err)
	}
}

func TestSESDeliveryComplaintOpen(t *testing.T) {
	cases := []struct {
		body string
		typ  event.Type
	}{
		{`{"eventType":"Delivery","delivery":{"recipients":["user5@outlook.com"]},"mail":{"destination":["user5@outlook.com"]}}`, event.Reply},
		{`{"notificationType":"Complaint","complaint":{"complainedRecipients":[{"emailAddress":"user6@yahoo.com"}]},"mail":{}}`, event.Complaint},
		{`{"eventType":"Open","mail":{"destination":["user7@gmail.com"]}}`, event.Open},
		{`{"eventType":"Send","mail":{"destination":["user8@gmail.com"]}}`, event.Attempt},
	}
	for _, tc := range cases {
		got, err := SES([]byte(tc.body), now)
		if err != nil || len(got) != 1 || got[0].Type != tc.typ {
			t.Errorf("SES(%s) = %+v, %v, want one %s", tc.body, got, err, tc.typ)
			continue
		}
		if got[0].Time != now {
			t.Errorf("undated notification time = %v, want now", got[0].Time)
		}
		if err := got[0].Validate(); err != nil {
			t.Error(err)
		}
	}
}

func TestSESIgnoresSubscriptionAndUnknown(t *testing.T) {
	got, err := SES([]byte(`{"Type":"SubscriptionConfirmation","Message":"x"}`), now)
	if err != nil || len(got) != 0 {
		t.Errorf("subscription confirmation = %+v, %v", got, err)
	}
	got, err = SES([]byte(`{"eventType":"Click","mail":{"destination":["a@b.c"]}}`), now)
	if err != nil || len(got) != 0 {
		t.Errorf("click = %+v, %v, want nothing", got, err)
	}
	if _, err := SES([]byte(`{"mail":{}}`), now); err == nil {
		t.Error("notification without a type accepted")
	}
	if _, err := SES([]byte(`not json`), now); err == nil {
		t.Error("bad json accepted")
	}
}

func TestSendGrid(t *testing.T) {
	body := `[
	 {"email":"user5@gmail.com","timestamp":1773129600,"event":"processed"},
	 {"email":"user5@gmail.com","timestamp":1773129601,"event":"delivered"},
	 {"email":"user6@outlook.com","timestamp":1773129602,"event":"deferred","status":"4.2.2"},
	 {"email":"user7@yahoo.com","timestamp":1773129603,"event":"bounce","type":"bounce"},
	 {"email":"user8@yahoo.com","timestamp":1773129604,"event":"bounce","type":"blocked"},
	 {"email":"user9@gmail.com","timestamp":1773129605,"event":"dropped"},
	 {"email":"user10@gmail.com","timestamp":1773129606,"event":"spamreport"},
	 {"email":"user11@gmail.com","timestamp":1773129607,"event":"open"},
	 {"email":"user12@gmail.com","timestamp":1773129608,"event":"click"}
	]`
	got, err := SendGrid([]byte(body), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		typ   event.Type
		code  int
		kind  event.BounceKind
		label string
	}{
		{event.Attempt, 0, "", ""},
		{event.Reply, 250, "", "250 2.0.0"},
		{event.Reply, 450, "", "450 4.2.2"},
		{event.Bounce, 0, event.Hard, ""},
		{event.Bounce, 0, event.Soft, ""},
		{event.Complaint, 0, "", ""},
		{event.Open, 0, "", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %d, want %d (dropped and click ignored)", len(got), len(want))
	}
	for i, w := range want {
		e := got[i]
		if e.Type != w.typ || e.Code != w.code || e.Bounce != w.kind || (w.label != "" && e.CodeLabel() != w.label) {
			t.Errorf("event %d = %+v, want %+v", i, e, w)
		}
		if e.Route != SourceSendGrid || e.Source != SourceSendGrid {
			t.Errorf("event %d route/source = %s/%s", i, e.Route, e.Source)
		}
		if err := e.Validate(); err != nil {
			t.Errorf("event %d: %v", i, err)
		}
	}
	if got[0].Time != time.Unix(1773129600, 0).UTC() {
		t.Errorf("time = %v", got[0].Time)
	}
}

func TestSESSkipsSuppressedAddresses(t *testing.T) {
	for _, body := range []string{
		`{"eventType":"Bounce","bounce":{"bounceType":"Permanent","bounceSubType":"OnAccountSuppressionList","bouncedRecipients":[{"emailAddress":"user5@gmail.com"}]},"mail":{}}`,
		`{"eventType":"Bounce","bounce":{"bounceType":"Permanent","bounceSubType":"Suppressed","bouncedRecipients":[{"emailAddress":"user5@gmail.com"}]},"mail":{}}`,
		`{"eventType":"Complaint","complaint":{"complaintSubType":"OnAccountSuppressionList","complainedRecipients":[{"emailAddress":"user5@gmail.com"}]},"mail":{}}`,
	} {
		if got, err := SES([]byte(body), now); err != nil || len(got) != 0 {
			t.Errorf("SES(%s) = %+v, %v, want nothing", body, got, err)
		}
	}
}

func TestSESDatesEventsByTheirOwnTimestamp(t *testing.T) {
	body := `{"eventType":"Bounce","bounce":{"bounceType":"Transient","timestamp":"2026-03-11T09:00:00Z","bouncedRecipients":[{"emailAddress":"user5@gmail.com"}]},"mail":{"timestamp":"2026-03-10T08:00:00Z"}}`
	got, err := SES([]byte(body), now)
	if err != nil || len(got) != 1 {
		t.Fatalf("SES() = %+v, %v", got, err)
	}
	if want := time.Date(2026, 3, 11, 9, 0, 0, 0, time.UTC); !got[0].Time.Equal(want) {
		t.Errorf("time = %v, want the bounce time %v", got[0].Time, want)
	}
}

func TestSESOpenIsOneEvent(t *testing.T) {
	got, err := SES([]byte(`{"eventType":"Open","mail":{"destination":["user5@gmail.com","user6@outlook.com","user7@yahoo.com"]}}`), now)
	if err != nil || len(got) != 1 || got[0].Domain != "" {
		t.Errorf("open to 3 recipients = %+v, %v, want one event without a domain", got, err)
	}
}

func TestSNSSubscription(t *testing.T) {
	body := `{"Type":"SubscriptionConfirmation","TopicArn":"arn:aws:sns:us-east-1:123456789012:ehlo","SubscribeURL":"https://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription&Token=x"}`
	sub, ok := SNSSubscription([]byte(body))
	if !ok || sub.SubscribeURL == "" || sub.TopicArn == "" {
		t.Errorf("SNSSubscription() = %+v, %v", sub, ok)
	}
	if _, ok := SNSSubscription([]byte(`{"Type":"Notification","Message":"{}"}`)); ok {
		t.Error("notification taken for a subscription")
	}
	if got, err := SES([]byte(`{"Type":"UnsubscribeConfirmation"}`), now); err != nil || len(got) != 0 {
		t.Errorf("unsubscribe confirmation = %+v, %v, want nothing", got, err)
	}
}

func TestSendGridDeferredKeepsProviderReply(t *testing.T) {
	body := `[{"email":"user5@yahoo.com","timestamp":1773129602,"event":"deferred","response":"421 4.7.0 [TSS04] Messages from 203.0.113.1 temporarily deferred"}]`
	got, err := SendGrid([]byte(body), now)
	if err != nil || len(got) != 1 {
		t.Fatalf("SendGrid() = %+v, %v", got, err)
	}
	e := got[0]
	if e.Code != 421 || e.Enhanced != "4.7.0" || e.Text == "" {
		t.Errorf("deferred = %+v, want 421 4.7.0 with the reply text", e)
	}
	if err := e.Validate(); err != nil {
		t.Error(err)
	}
}

func TestSESRecipientListOverTheEventLimitIsRejected(t *testing.T) {
	rcpts := func(n int) string {
		return `"` + strings.Repeat(`user@gmail.com","`, n-1) + `user@gmail.com"`
	}
	bounced := func(n int) string {
		return strings.Repeat(`{"emailAddress":"user@gmail.com"},`, n-1) + `{"emailAddress":"user@gmail.com"}`
	}
	cases := map[string]func(n int) string{
		"delivery": func(n int) string {
			return `{"eventType":"Delivery","delivery":{"recipients":[` + rcpts(n) + `]},"mail":{}}`
		},
		"send": func(n int) string { return `{"eventType":"Send","mail":{"destination":[` + rcpts(n) + `]}}` },
		"bounce": func(n int) string {
			return `{"eventType":"Bounce","bounce":{"bounceType":"Permanent","bouncedRecipients":[` + bounced(n) + `]},"mail":{}}`
		},
		"complaint": func(n int) string {
			return `{"eventType":"Complaint","complaint":{"complainedRecipients":[` + bounced(n) + `]},"mail":{}}`
		},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := SES([]byte(body(event.MaxBatch)), now)
			if err != nil || len(got) != event.MaxBatch {
				t.Fatalf("%d recipients: %d events, %v, want %d events", event.MaxBatch, len(got), err, event.MaxBatch)
			}
			got, err = SES([]byte(body(event.MaxBatch+1)), now)
			if !errors.Is(err, event.ErrBatchTooLarge) || got != nil {
				t.Errorf("%d recipients: %d events, %v, want ErrBatchTooLarge", event.MaxBatch+1, len(got), err)
			}
		})
	}
}
