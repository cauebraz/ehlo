package postfix

import (
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/event"
)

var fixed = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func parser() *Parser {
	return NewParser("mx-1", WithClock(func() time.Time { return fixed }), WithYear(2026), WithLocation(time.UTC))
}

// deliveryLine builds one smtp(8) delivery line for user1@gmail.com at
// 08:15:02 with delay 1.2, varying only the dsn, status and reply text.
func deliveryLine(dsn, status, text string) string {
	return `Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user1@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=1.2, delays=0.01/0/0.5/0.7, dsn=` +
		dsn + `, status=` + status + ` (` + text + `)`
}

func TestDeliveryLines(t *testing.T) {
	cases := []struct {
		name         string
		line         string
		wantEvents   int
		wantCode     int
		wantEnhanced string
		wantStage    event.Stage
		wantClass    event.Class
		wantLabel    string
		wantBounce   event.BounceKind
		wantAttempt  event.Attempted
		wantTag      string
	}{
		{
			name:       "sent",
			line:       deliveryLine("2.0.0", "sent", "250 2.0.0 OK  1710058502 x12si345678wrs.123 - gsmtp"),
			wantEvents: 2, wantCode: 250, wantEnhanced: "2.0.0", wantStage: event.StageData, wantClass: "2xx", wantLabel: "250 2.0.0", wantAttempt: event.First,
		},
		{
			name:       "deferred at rcpt",
			line:       deliveryLine("4.7.28", "deferred", "host gmail-smtp-in.l.google.com[203.0.113.26] said: 421-4.7.28 Our system has detected an unusual rate of unsolicited mail (in reply to RCPT TO command)"),
			wantEvents: 2, wantCode: 421, wantEnhanced: "4.7.28", wantStage: event.StageRcpt, wantClass: "4xx", wantLabel: "421 4.7.28", wantAttempt: event.First,
		},
		{
			name:       "deferred connection failure",
			line:       deliveryLine("4.4.1", "deferred", "connect to outlook-com.olc.protection.outlook.com[203.0.113.33]:25: Connection timed out"),
			wantEvents: 2, wantCode: 0, wantEnhanced: "", wantStage: event.StageConnect, wantClass: "connect", wantLabel: "connect", wantAttempt: event.First,
		},
		{
			name:       "greeting timeout is connection level",
			line:       deliveryLine("4.4.2", "deferred", "conversation with outlook-com.olc.protection.outlook.com[203.0.113.33] timed out while receiving the initial server greeting"),
			wantEvents: 2, wantCode: 0, wantEnhanced: "", wantStage: event.StageConnect, wantClass: "connect", wantLabel: "connect", wantAttempt: event.First,
		},
		{
			name:       "lost connection in data",
			line:       deliveryLine("4.4.2", "deferred", "lost connection with gmail-smtp-in.l.google.com[203.0.113.26] while sending end of data -- message may be sent more than once"),
			wantEvents: 2, wantCode: 450, wantEnhanced: "4.4.2", wantStage: event.StageData, wantClass: "4xx", wantLabel: "450 4.4.2", wantAttempt: event.First,
		},
		{
			name:       "vendor tag at mail from",
			line:       deliveryLine("4.7.0", "deferred", "host mta7.am0.yahoodns.net[203.0.113.110] said: 421 4.7.0 [TSS04] Messages from 203.0.113.1 temporarily deferred due to unexpected volume or user complaints (in reply to MAIL FROM command)"),
			wantEvents: 2, wantCode: 421, wantEnhanced: "4.7.0", wantStage: event.StageMail, wantClass: "4xx", wantLabel: "421 4.7.0 TSS04", wantAttempt: event.First, wantTag: "TSS04",
		},
		{
			name:       "softbounce keeps the provider's code",
			line:       deliveryLine("4.1.1", "SOFTBOUNCE", "host gmail-smtp-in.l.google.com[203.0.113.26] said: 550 5.1.1 The email account that you tried to reach does not exist (in reply to RCPT TO command)"),
			wantEvents: 2, wantCode: 550, wantEnhanced: "5.1.1", wantStage: event.StageRcpt, wantClass: "5xx", wantLabel: "550 5.1.1", wantAttempt: event.First,
		},
		{
			name:       "policy bounce",
			line:       deliveryLine("5.7.1", "bounced", "host mta7.am0.yahoodns.net[203.0.113.110] said: 554 5.7.1 [TSS04] Messages from 203.0.113.1 temporarily deferred (in reply to MAIL FROM command)"),
			wantEvents: 3, wantCode: 554, wantEnhanced: "5.7.1", wantStage: event.StageMail, wantClass: "5xx", wantLabel: "554 5.7.1 TSS04", wantBounce: event.Policy, wantAttempt: event.First, wantTag: "TSS04",
		},
		{
			name:       "hard bounce",
			line:       deliveryLine("5.1.1", "bounced", "host gmail-smtp-in.l.google.com[203.0.113.26] said: 550-5.1.1 The email account that you tried to reach does not exist (in reply to RCPT TO command)"),
			wantEvents: 3, wantCode: 550, wantEnhanced: "5.1.1", wantStage: event.StageRcpt, wantClass: "5xx", wantLabel: "550 5.1.1", wantBounce: event.Hard, wantAttempt: event.First,
		},
	}
	wantTime := time.Date(2026, 3, 10, 8, 15, 2, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parser().Parse(tc.line)
			if len(got) != tc.wantEvents {
				t.Fatalf("events = %d, want %d: %+v", len(got), tc.wantEvents, got)
			}
			a, r := got[0], got[1]
			if a.Type != event.Attempt || a.Domain != "gmail.com" || a.Source != "mx-1" || a.Attempt != tc.wantAttempt {
				t.Errorf("attempt = %+v", a)
			}
			if r.Type != event.Reply || r.Code != tc.wantCode || r.Enhanced != tc.wantEnhanced || r.Stage != tc.wantStage || r.Delay != 1.2 || r.Text == "" {
				t.Errorf("reply = %+v, want %d %q at %s", r, tc.wantCode, tc.wantEnhanced, tc.wantStage)
			}
			if r.Class() != tc.wantClass || r.Tag != tc.wantTag || r.CodeLabelWithTag() != tc.wantLabel {
				t.Errorf("class/tag/label = %q %q %q, want %q %q %q", r.Class(), r.Tag, r.CodeLabelWithTag(), tc.wantClass, tc.wantTag, tc.wantLabel)
			}
			if !r.Time.Equal(wantTime) {
				t.Errorf("time = %v, want %v", r.Time, wantTime)
			}
			if tc.wantEvents == 3 {
				if b := got[2]; b.Type != event.Bounce || b.Bounce != tc.wantBounce {
					t.Errorf("bounce = %+v, want %s", b, tc.wantBounce)
				}
			}
		})
	}
}

func TestExpiredComesFromQmgr(t *testing.T) {
	line := `Mar 11 08:17:00 mx-1 postfix/qmgr[1200]: 4F3A1B2C52: from=<bounce@example.com>, status=expired, returned to sender`
	got := parser().Parse(line)
	if len(got) != 1 || got[0].Type != event.Bounce || got[0].Bounce != event.Soft {
		t.Errorf("expired events = %+v, want one soft bounce", got)
	}
	smtpExpired := `Mar 11 08:17:00 mx-1 postfix/smtp[2250]: 4F3A1B2C52: to=<user9@gmail.com>, relay=none, delay=86500, delays=86500/0/0/0, dsn=4.4.7, status=expired (delivery temporarily suspended)`
	if got := parser().Parse(smtpExpired); got != nil {
		t.Errorf("smtp expired line produced %+v, want nil (qmgr logs the expiry)", got)
	}
}

func TestTLSLines(t *testing.T) {
	got := parser().Parse(`Mar 10 08:19:00 mx-1 postfix/smtp[2261]: Untrusted TLS connection established to gmail-smtp-in.l.google.com[203.0.113.26]:25: TLSv1.3 with cipher TLS_AES_256_GCM_SHA384 (256/256 bits) key-exchange X25519 server-signature ECDSA (P-256) server-digest SHA256`)
	if len(got) != 1 || got[0].Type != event.TLS || got[0].TLSVersion != "TLSv1.3" || got[0].TLSTrust != "untrusted" {
		t.Errorf("tls established events = %+v", got)
	}
	got = parser().Parse(`2026-03-10T08:19:00.123456+00:00 mx-1 postfix/smtp[2261]: SSL_connect error to mx.example.com[203.0.113.11]:25: -1`)
	if len(got) != 1 || got[0].Stage != event.StageTLS || !got[0].Time.Equal(time.Date(2026, 3, 10, 8, 19, 0, 123456000, time.UTC)) {
		t.Errorf("tls failure events = %+v", got)
	}
}

func TestFormatsThatUsedToDropEverything(t *testing.T) {
	lines := map[string]string{
		"long queue id":     `Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 3Ps0FS1Zhtz1PFjb: to=<user5@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.4]:25, delay=0.5, delays=0/0/0.2/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK)`,
		"postfix 3.11 tls=": `Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user5@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.4]:25, conn_use=2, delay=0.5, delays=0/0/0.2/0.3, tls=TLSv1.3:TLS_AES_256_GCM_SHA384, dsn=2.0.0, status=sent (250 2.0.0 OK)`,
		"multi-instance":    `Mar 10 08:15:02 mx-1 postfix-out/smtp[2231]: 4F3A1B2C3D: to=<user5@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.4]:25, delay=0.5, delays=0/0/0.2/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK)`,
	}
	for name, line := range lines {
		got := parser().Parse(line)
		if len(got) != 2 || got[1].Code != 250 || got[1].Delay != 0.5 {
			t.Errorf("%s: events = %+v, want attempt and 250 reply with delay 0.5", name, got)
		}
	}
}

func TestIgnoredLines(t *testing.T) {
	for _, line := range []string{
		`Mar 10 08:20:00 mx-1 postfix/qmgr[1200]: 4F3A1B2C60: from=<bounce@example.com>, size=1200, nrcpt=1 (queue active)`,
		`Mar 10 08:20:01 mx-1 postfix/smtpd[1300]: connect from localhost[127.0.0.1]`,
		`Mar 10 08:18:00 mx-1 postfix/smtp[2260]: connect to mx1.example.co.uk[203.0.113.10]:25: Connection refused`,
		``,
	} {
		if got := parser().Parse(line); got != nil {
			t.Errorf("Parse(%q) = %+v, want nil", line, got)
		}
	}
}

func TestEveryEventValidates(t *testing.T) {
	lines := []string{
		`Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user5@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.4]:25, delay=0.5, delays=0/0/0.2/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK)`,
		`Mar 10 08:16:00 mx-1 postfix/smtp[2240]: 4F3A1B2C40: to=<user7@outlook.com>, relay=none, delay=30, delays=0/0/30/0, dsn=4.4.1, status=deferred (connect to x[203.0.113.4]:25: Connection timed out)`,
		`Mar 10 08:19:00 mx-1 postfix/smtp[2261]: Trusted TLS connection established to mx.example.com[203.0.113.11]:25: TLSv1.2 with cipher ECDHE-RSA-AES256-GCM-SHA384 (256/256 bits)`,
	}
	for _, line := range lines {
		for _, e := range parser().Parse(line) {
			if err := e.Validate(); err != nil {
				t.Errorf("%q produced an invalid event %+v: %v", line, e, err)
			}
		}
	}
}

func TestRetryIsPerRecipient(t *testing.T) {
	p := parser()
	first := p.Parse(`Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user1@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=0.52, delays=0.01/0/0.21/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK)`)
	second := p.Parse(`Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user2@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=0.52, delays=0.01/0/0.21/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK)`)
	again := p.Parse(`Mar 10 08:25:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user2@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=600, delays=0.01/600/0.21/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK)`)
	if first[0].Attempt != event.First || second[0].Attempt != event.First {
		t.Errorf("two recipients of one message = %s and %s, want both first", first[0].Attempt, second[0].Attempt)
	}
	if again[0].Attempt != event.Retry {
		t.Errorf("same recipient again = %s, want retry", again[0].Attempt)
	}
	active := p.Parse(`Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3F: to=<user3@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=100, delays=0/96/0/4, dsn=2.0.0, status=sent (250 2.0.0 OK)`)
	if active[0].Attempt != event.First {
		t.Errorf("96s waiting for a delivery slot in the active queue = %s, want first", active[0].Attempt)
	}
	deferred := p.Parse(`Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C40: to=<user4@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=301, delays=300/0.5/0.21/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK)`)
	if deferred[0].Attempt != event.Retry {
		t.Errorf("five minutes before the last active queue entry, first line unseen = %s, want retry", deferred[0].Attempt)
	}
}

func TestLocalAgentsAndSmarthostsAreIgnored(t *testing.T) {
	p := NewParser("mx-1", WithClock(func() time.Time { return fixed }), WithYear(2026), WithLocation(time.UTC), WithSkipRelays([]string{"smarthost.example.org"}))
	for _, line := range []string{
		`Mar 10 08:19:00 mx-1 postfix/lmtp[2261]: Untrusted TLS connection established to dovecot[private/dovecot-lmtp]: TLSv1.3 with cipher TLS_AES_256_GCM_SHA384 (256/256 bits)`,
		`Mar 10 08:19:00 mx-1 postfix/lmtp[2261]: SSL_connect error to dovecot[private/dovecot-lmtp]: -1`,
		`Mar 10 08:15:02 mx-1 postfix/relay[2231]: 4F3A1B2C3D: to=<user1@gmail.com>, relay=smarthost.example.org[203.0.113.9]:587, delay=0.52, delays=0.01/0/0.21/0.3, dsn=2.0.0, status=sent (250 2.0.0 Ok: queued as 1234)`,
	} {
		if got := p.Parse(line); len(got) != 0 {
			t.Errorf("%q produced %+v", line, got)
		}
	}
}

func TestGreetingRefusalIsAPolicyBounce(t *testing.T) {
	line := `Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user1@t-online.de>, relay=none, delay=1.2, delays=0.01/0/0.5/0, dsn=5.0.0, status=bounced (host mx00.t-online.de[203.0.113.40] refused to talk to me: 554 IP=203.0.113.1 - A problem occurred. (Ask your postmaster for help or to contact tosa@rx.t-online.de to clarify.) (BL))`
	got := parser().Parse(line)
	if len(got) != 3 {
		t.Fatalf("events = %d, want attempt, reply, bounce", len(got))
	}
	if r := got[1]; r.Code != 554 || r.Stage != event.StageConnect {
		t.Errorf("reply = code %d stage %s, want 554 at connect", r.Code, r.Stage)
	}
	if b := got[2]; b.Bounce != event.Policy {
		t.Errorf("bounce = %s, want policy", b.Bounce)
	}
}

func TestReplyToEHLOKeepsProviderCode(t *testing.T) {
	got := parser().Parse(deliveryLine("4.7.0", "deferred", "host outlook-com.olc.protection.outlook.com[203.0.113.33] said: 421 4.7.0 too many connections RP-003 (in reply to EHLO command)"))
	if len(got) != 2 {
		t.Fatalf("events = %d", len(got))
	}
	if r := got[1]; r.Code != 421 || r.Enhanced != "4.7.0" || r.Stage != event.StageEHLO || r.Tag == "" {
		t.Errorf("reply = %+v, want 421 4.7.0 at ehlo with a tag", r)
	}
}

func TestConnectInsideProviderReplyKeepsCode(t *testing.T) {
	got := parser().Parse(deliveryLine("4.7.1", "deferred", "host gmail-smtp-in.l.google.com[203.0.113.26] said: 450 4.7.1 cannot connect to your SMTP port (in reply to RCPT TO command)"))
	if r := got[1]; r.Code != 450 || r.Stage != event.StageRcpt {
		t.Errorf("reply = code %d stage %s, want 450 at rcpt", r.Code, r.Stage)
	}
}

func TestRetryServiceIsIgnored(t *testing.T) {
	line := `Mar 10 08:15:02 mx-1 postfix/retry[2231]: 4F3A1B2C3D: to=<user1@gmail.com>, relay=none, delay=1.2, delays=0.01/0/0.5/0, dsn=4.4.1, status=deferred (delivery temporarily suspended: connect to gmail-smtp-in.l.google.com[203.0.113.26]:25: Connection timed out)`
	if got := parser().Parse(line); len(got) != 0 {
		t.Errorf("retry(8) line = %+v, want nothing", got)
	}
}

func TestRFC5424Line(t *testing.T) {
	line := `<22>1 2026-03-10T08:15:02Z mx-1 postfix/smtp 2231 - - 4F3A1B2C3D: to=<user1@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=1.2, delays=0.01/0/0.5/0.7, dsn=2.0.0, status=sent (250 2.0.0 OK)`
	got := parser().Parse(line)
	if len(got) != 2 || !got[0].Time.Equal(time.Date(2026, 3, 10, 8, 15, 2, 0, time.UTC)) {
		t.Errorf("RFC 5424 line = %+v, want 2 events at 08:15:02", got)
	}
}

func TestLeapDayFromLastYear(t *testing.T) {
	p := NewParser("mx-1", WithClock(func() time.Time { return time.Date(2029, 1, 10, 0, 0, 0, 0, time.UTC) }), WithLocation(time.UTC))
	line := `Feb 29 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user1@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=1.2, delays=0.01/0/0.5/0.7, dsn=2.0.0, status=sent (250 2.0.0 OK)`
	got := p.Parse(line)
	if len(got) == 0 || !got[0].Time.Equal(time.Date(2028, 2, 29, 8, 15, 2, 0, time.UTC)) {
		t.Errorf("leap day = %+v, want 2028-02-29", got)
	}
}

// TestJournalctlShortISO reads lines as journalctl -o short-iso prints
// them. The +0000 lines were captured from Postfix 3.7.11 under systemd 252
// on Debian 12 without rsyslog; newer systemd releases print the offset
// with a colon.
func TestJournalctlShortISO(t *testing.T) {
	cases := []struct {
		line  string
		types []event.Type
		code  int
		time  time.Time
	}{
		{
			line:  `2026-11-16T23:39:52+0000 mx-1 postfix/smtp[349]: 912C1684F7E: to=<user1@gmail.com>, relay=mx.gmail.example[203.0.113.26]:25, delay=30, delays=30/0/0/0, dsn=2.0.0, status=sent (250 2.0.0 Ok)`,
			types: []event.Type{event.Attempt, event.Reply},
			code:  250,
			time:  time.Date(2026, 11, 16, 23, 39, 52, 0, time.UTC),
		},
		{
			line:  `2026-11-16T23:39:52+0000 mx-1 postfix/smtp[293]: B3E18684F8C: to=<user6@yahoo.com>, relay=mx.yahoo.example[203.0.113.27]:25, delay=12, delays=12/0/0/0, dsn=4.3.0, status=deferred (host mx.yahoo.example[203.0.113.27] said: 450 4.3.0 Error: command failed (in reply to RCPT TO command))`,
			types: []event.Type{event.Attempt, event.Reply},
			code:  450,
			time:  time.Date(2026, 11, 16, 23, 39, 52, 0, time.UTC),
		},
		{
			line:  `2026-11-16T23:39:52+0000 mx-1 postfix/smtp[267]: B4B0F684F8E: to=<user7@outlook.com>, relay=mx.outlook.example[203.0.113.28]:25, delay=12, delays=12/0/0/0, dsn=5.3.0, status=bounced (host mx.outlook.example[203.0.113.28] said: 500 5.3.0 Error: command failed (in reply to RCPT TO command))`,
			types: []event.Type{event.Attempt, event.Reply, event.Bounce},
			code:  500,
			time:  time.Date(2026, 11, 16, 23, 39, 52, 0, time.UTC),
		},
		{
			line:  `2026-11-16T23:39:22+0000 mx-1 postfix/smtp[269]: 91CDD684F80: to=<user2@yahoo.com>, relay=none, delay=0.01, delays=0/0.01/0/0, dsn=4.4.1, status=deferred (connect to mx.yahoo.example[203.0.113.27]:25: Connection refused)`,
			types: []event.Type{event.Attempt, event.Reply},
			time:  time.Date(2026, 11, 16, 23, 39, 22, 0, time.UTC),
		},
		{
			line:  `2026-11-16T20:39:52-03:00 mx-1 postfix/smtp[349]: 912C1684F7E: to=<user1@gmail.com>, relay=mx.gmail.example[203.0.113.26]:25, delay=30, delays=30/0/0/0, dsn=2.0.0, status=sent (250 2.0.0 Ok)`,
			types: []event.Type{event.Attempt, event.Reply},
			code:  250,
			time:  time.Date(2026, 11, 16, 23, 39, 52, 0, time.UTC),
		},
		{
			line: `2026-11-16T23:39:22+0000 mx-1 postfix/qmgr[255]: 912C1684F7E: from=<sender@mx-1.example>, size=263, nrcpt=1 (queue active)`,
		},
	}
	for _, tc := range cases {
		got := parser().Parse(tc.line)
		if len(got) != len(tc.types) {
			t.Errorf("Parse(%q) = %d events, want %d", tc.line, len(got), len(tc.types))
			continue
		}
		for i, e := range got {
			if e.Type != tc.types[i] || !e.Time.Equal(tc.time) {
				t.Errorf("event %d = %s at %v, want %s at %v", i, e.Type, e.Time, tc.types[i], tc.time)
			}
			if e.Type == event.Reply && e.Code != tc.code {
				t.Errorf("reply code = %d, want %d", e.Code, tc.code)
			}
			if err := e.Validate(); err != nil {
				t.Error(err)
			}
		}
	}
}
