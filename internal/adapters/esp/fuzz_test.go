package esp

import (
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/event"
)

var fuzzNow = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func checkEvents(t *testing.T, events []event.Event) {
	t.Helper()
	for _, e := range events {
		if e.Time.IsZero() || e.Source == "" {
			t.Errorf("event %+v: want a time and a source", e)
		}
		if len(e.Text) > event.MaxTextLength {
			t.Errorf("text is %d bytes, above %d", len(e.Text), event.MaxTextLength)
		}
	}
}

func FuzzSES(f *testing.F) {
	for _, s := range []string{
		`{"eventType":"Bounce","bounce":{"bounceType":"Permanent","bouncedRecipients":[{"emailAddress":"user5@gmail.com"}]}}`,
		`{"Type":"Notification","Message":"{\"eventType\":\"Delivery\",\"delivery\":{\"recipients\":[\"user5@yahoo.com\"]}}"}`,
		`{"Type":"SubscriptionConfirmation","SubscribeURL":"https://sns.example.com/confirm"}`,
		`{"notificationType":"Complaint","complaint":{"complainedRecipients":[{"emailAddress":"user6@outlook.com"}]}}`,
		`not json`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		events, err := SES(body, fuzzNow)
		if err == nil {
			checkEvents(t, events)
		}
		_, _ = SNSSubscription(body)
	})
}

func FuzzSendGrid(f *testing.F) {
	for _, s := range []string{
		`[{"email":"user5@gmail.com","timestamp":1710058502,"event":"delivered"}]`,
		`[{"email":"user5@yahoo.com","event":"deferred","response":"421 4.7.0 [TSS04] try later","status":"4.7.0"}]`,
		`[{"email":"user5@outlook.com","event":"bounce","type":"blocked"}]`,
		`{}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		events, err := SendGrid(body, fuzzNow)
		if err == nil {
			checkEvents(t, events)
		}
	})
}
