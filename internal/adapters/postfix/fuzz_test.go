package postfix

import (
	"testing"
	"time"

	"github.com/cauebraz/ehlo/internal/event"
)

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`Mar 10 08:15:02 mx-1 postfix/smtp[2231]: 4F3A1B2C3D: to=<user1@gmail.com>, relay=gmail-smtp-in.l.google.com[203.0.113.26]:25, delay=0.52, delays=0.01/0/0.21/0.3, dsn=2.0.0, status=sent (250 2.0.0 OK)`,
		`2026-03-10T08:15:03.123+00:00 mx-1 postfix/smtp[2231]: 4F3A1B2C3E: to=<user2@yahoo.com>, relay=mta5.am0.yahoodns.net[203.0.113.27]:25, delay=1.2, delays=0.01/0/0.5/0.7, dsn=4.7.0, status=deferred (host mta5.am0.yahoodns.net[203.0.113.27] said: 421 4.7.0 [TSS04] Messages temporarily deferred (in reply to MAIL FROM command))`,
		`<22>1 2026-03-10T08:15:04Z mx-1 postfix/smtp 2231 - - Verified TLS connection established to mx.example.net[203.0.113.28]:25: TLSv1.3 with cipher TLS_AES_256_GCM_SHA384`,
		`Mar 10 08:15:05 mx-1 postfix/qmgr[2200]: 4F3A1B2C3F: from=<a@example.org>, status=expired, returned to sender`,
		`Feb 29 00:00:00 mx-1 postfix/smtp[1]: Cannot start TLS: handshake failure`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		p := NewParser("mx-1", WithClock(func() time.Time { return time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC) }), WithLocation(time.UTC))
		for _, e := range p.Parse(line) {
			if e.Source != "mx-1" || e.Time.IsZero() {
				t.Errorf("event %+v: want source mx-1 and a time", e)
			}
			if len(e.Text) > event.MaxTextLength {
				t.Errorf("text is %d bytes, above %d", len(e.Text), event.MaxTextLength)
			}
			if e.Tag != "" && !event.ValidTag(e.Tag) {
				t.Errorf("tag %q fails event validation", e.Tag)
			}
		}
	})
}
