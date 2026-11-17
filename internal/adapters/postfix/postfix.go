// Package postfix translates Postfix smtp(8) log lines into events.
package postfix

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cauebraz/ehlo/internal/classify"
	"github.com/cauebraz/ehlo/internal/event"
)

// daemonFor returns the syslog program prefix of one Postfix service, with
// the multi-instance (postfix-out) and chrooted (postfix/submission) forms.
func daemonFor(svc string) string {
	return `(?:postfix|postfix-[\w.-]+)(?:/[\w-]+)?/` + svc + `\[\d+\]: `
}

const number = `\d+(?:\.\d+)?`

var (
	// daemon matches any master.cf service name, so a per-provider transport
	// such as smtp-gmail or relay matches as well as smtp. The local delivery
	// agents are filtered through localService after the match.
	daemon     = daemonFor(`(?P<svc>[\w-]+)`)
	queueID    = `(?P<id>[0-9A-F]{6,}|[0-9A-Za-z]{10,}): `
	deliveryRe = regexp.MustCompile(daemon + queueID +
		`to=<(?P<rcpt>[^>]*@(?P<domain>[^>]+))>,(?: orig_to=<[^>]*>,)? relay=(?P<relay>[^,]+),(?: conn_use=\d+,)? ` +
		`delay=(?P<delay>` + number + `), delays=(?P<before>` + number + `)/` + number + `/` + number + `/` + number + `,` +
		`(?: tls=[^,]+,)? dsn=(?P<dsn>[\d.]+), status=(?P<status>\w+) \((?P<text>.*)\)$`)
	enhancedRe = regexp.MustCompile(`\b([245]\.\d{1,3}\.\d{1,3})\b`)
	expiredRe  = regexp.MustCompile(daemonFor("qmgr") + queueID + `from=<[^>]*>, status=expired, returned to sender`)
	tlsOKRe    = regexp.MustCompile(daemon + `(?P<trust>Anonymous|Untrusted|Trusted|Verified) TLS connection established to \S+: (?P<version>TLSv1(?:\.[123])?|SSLv3) with cipher`)
	codeRe     = regexp.MustCompile(`(?:said: |refused to talk to me: |^)([245]\d\d)[ -]`)
	remoteRe   = regexp.MustCompile(`(?:said|refused to talk to me): [245]\d\d[ -]`)
	rfc5424Re  = regexp.MustCompile(`^<\d{1,3}>1 (\S+) (\S+) (\S+) (\S+) \S+ (?:-|\[.*?\]) (.*)$`)
	priRe      = regexp.MustCompile(`^<\d{1,3}>`)
	syslogTime = regexp.MustCompile(`^(\w{3} {1,2}\d{1,2} \d{2}:\d{2}:\d{2})\s`)
	isoTime    = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T[\d:.]+(?:Z|[+-]\d{2}:?\d{2}))\s`)
)

var (
	svcGroup     = deliveryRe.SubexpIndex("svc")
	idGroup      = deliveryRe.SubexpIndex("id")
	rcptGroup    = deliveryRe.SubexpIndex("rcpt")
	domainGroup  = deliveryRe.SubexpIndex("domain")
	relayGroup   = deliveryRe.SubexpIndex("relay")
	delayGroup   = deliveryRe.SubexpIndex("delay")
	beforeGroup  = deliveryRe.SubexpIndex("before")
	dsnGroup     = deliveryRe.SubexpIndex("dsn")
	statusGroup  = deliveryRe.SubexpIndex("status")
	textGroup    = deliveryRe.SubexpIndex("text")
	tlsSvcGroup  = tlsOKRe.SubexpIndex("svc")
	trustGroup   = tlsOKRe.SubexpIndex("trust")
	versionGroup = tlsOKRe.SubexpIndex("version")
)

// Parser keeps the source identity and the clock a log line lacks. The zero
// value of every field below Source is valid, so both NewParser and a struct
// literal produce a working Parser.
type Parser struct {
	// Source is the identity written into every event, usually the host name.
	Source string
	// Now is the clock used for lines without a year and for lines without a
	// timestamp. Nil means time.Now.
	Now func() time.Time
	// Year is the year of syslog timestamps, which carry none. Zero means the
	// year of Now, moved back one when the line would otherwise be more than a
	// day in the future.
	Year int
	// Location is the zone of syslog timestamps, which carry none. Nil
	// means the host's local zone.
	Location *time.Location
	// RetryAfter is the time from arrival to the last active queue entry,
	// the first field of delays=, from which a delivery line is a retry.
	// Zero means one minute. Lines for a queue id already seen are retries
	// whatever their age.
	RetryAfter time.Duration
	// SkipRelays lists relay hosts whose replies are not a provider's, such
	// as a smarthost in front of this server. Their delivery lines are ignored.
	SkipRelays []string

	seen  map[string]struct{}
	order []string
}

// Option configures a Parser built by NewParser.
type Option func(*Parser)

// WithClock sets the clock used for undated lines and year inference.
func WithClock(now func() time.Time) Option { return func(p *Parser) { p.Now = now } }

// WithLocation sets the zone of syslog timestamps.
func WithLocation(loc *time.Location) Option { return func(p *Parser) { p.Location = loc } }

// WithRetryAfter sets the wait before the last active queue entry from which
// a delivery line is a retry.
func WithRetryAfter(d time.Duration) Option { return func(p *Parser) { p.RetryAfter = d } }

// WithSkipRelays sets the relay hosts whose delivery lines are ignored.
func WithSkipRelays(hosts []string) Option { return func(p *Parser) { p.SkipRelays = hosts } }

// WithYear fixes the year of syslog timestamps instead of inferring it.
func WithYear(year int) Option { return func(p *Parser) { p.Year = year } }

// NewParser returns a Parser for source with its defaults applied.
func NewParser(source string, opts ...Option) *Parser {
	p := &Parser{Source: source}
	for _, opt := range opts {
		opt(p)
	}
	p.defaults()
	return p
}

// defaults fills the implicit values once. It is idempotent and runs at the
// top of Parse so a struct literal Parser behaves like one from NewParser.
func (p *Parser) defaults() {
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.Location == nil {
		p.Location = time.Local
	}
	if p.RetryAfter == 0 {
		p.RetryAfter = time.Minute
	}
	if p.seen == nil {
		p.seen = make(map[string]struct{}, 1024)
	}
}

const seenIDs = 100_000

// localService names the Postfix delivery agents that never reach a
// mailbox provider.
// retry is error(8) under another service name: it reports a destination
// Postfix already marked down, without a new connection.
var localService = map[string]bool{"lmtp": true, "local": true, "virtual": true, "pipe": true, "discard": true, "error": true, "retry": true}

// Parse returns the events one log line describes, or nil for a line
// without delivery meaning.
func (p *Parser) Parse(line string) []event.Event {
	p.defaults()
	line = normalize(strings.TrimRight(line, "\r\n"))
	base := event.Event{Version: event.Version, Time: p.when(line), Source: p.Source}

	if m := deliveryRe.FindStringSubmatch(line); m != nil && !localService[m[svcGroup]] {
		return p.delivery(m, base)
	}
	if expiredRe.MatchString(line) {
		bounce := base
		bounce.Type, bounce.Bounce = event.Bounce, event.Soft
		return []event.Event{bounce}
	}
	if m := tlsOKRe.FindStringSubmatch(line); m != nil && !localService[m[tlsSvcGroup]] {
		tls := base
		tls.Type, tls.TLSTrust, tls.TLSVersion = event.TLS, strings.ToLower(m[trustGroup]), m[versionGroup]
		if tls.TLSVersion == "SSLv3" {
			tls.TLSVersion = "none"
		}
		return []event.Event{tls}
	}
	return nil
}

// delivery turns one matched delivery line into an attempt, the provider's
// reply and, for a bounced status, the bounce. The delay groups match only
// decimal numbers, so ParseFloat cannot fail on them.
func (p *Parser) delivery(m []string, base event.Event) []event.Event {
	if p.skipRelay(m[relayGroup]) {
		return nil
	}
	delay, _ := strconv.ParseFloat(m[delayGroup], 64)
	before, _ := strconv.ParseFloat(m[beforeGroup], 64)
	dsn, text := m[dsnGroup], m[textGroup]

	base.Domain = strings.ToLower(m[domainGroup])
	if local(m[relayGroup], text) {
		if m[statusGroup] != "bounced" {
			return nil
		}
		bounce := base
		bounce.Type, bounce.Bounce = event.Bounce, bounceKind(dsn, classify.Classify(classify.Reply{Code: codeFor(dsn, text), Enhanced: dsn, Text: text}))
		return []event.Event{bounce}
	}
	attempt := base
	attempt.Type, attempt.Attempt = event.Attempt, event.First
	if p.retry(m[idGroup]+"|"+strings.ToLower(m[rcptGroup]), before) {
		attempt.Attempt = event.Retry
	}
	reply := base
	reply.Type, reply.Enhanced, reply.Delay = event.Reply, dsn, delay
	reply.Code = codeFor(dsn, text)
	reply.Text = truncate(text)
	reply.Tag = classify.Tag(text)

	switch m[statusGroup] {
	case "sent":
		reply.Stage = event.StageData
		return []event.Event{attempt, reply}
	case "SOFTBOUNCE":
		if e := enhancedRe.FindStringSubmatch(text); e != nil {
			reply.Enhanced = e[1]
		}
		reply.Stage = stageOf(text)
		return []event.Event{attempt, reply}
	case "deferred":
		switch stage, ok := connectionStage(text); {
		case remoteRe.MatchString(text):
			reply.Stage = stageOf(text)
		case ok:
			reply.Stage, reply.Code, reply.Enhanced = stage, 0, ""
		default:
			reply.Stage = stageOf(text)
		}
		return []event.Event{attempt, reply}
	case "bounced":
		reply.Stage = stageOf(text)
		bounce := base
		bounce.Type, bounce.Bounce = event.Bounce, bounceKind(dsn, classify.Event(&reply))
		return []event.Event{attempt, reply, bounce}
	}
	return nil
}

// local reports a failure Postfix raised before any SMTP session, such as a
// resolver error or a mail loop: no relay, no remote reply and no
// connection failure. It says nothing about the provider.
func local(relay, text string) bool {
	if relay != "none" || remoteRe.MatchString(text) {
		return false
	}
	_, conn := connectionStage(text)
	return !conn
}

func truncate(s string) string {
	if len(s) > event.MaxTextLength {
		return s[:event.MaxTextLength]
	}
	return s
}

func (p *Parser) skipRelay(relay string) bool {
	host := strings.ToLower(relay)
	if i := strings.IndexAny(host, "[:"); i >= 0 {
		host = host[:i]
	}
	for _, s := range p.SkipRelays {
		if strings.EqualFold(s, host) {
			return true
		}
	}
	return false
}

// containsAny reports whether t contains at least one of needles.
func containsAny(t string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(t, n) {
			return true
		}
	}
	return false
}

// stageOf reads Postfix's "(in reply to X command)" suffix, the "while
// sending X" clause of a session that died, or a greeting refusal.
func stageOf(text string) event.Stage {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "refused to talk to me"):
		return event.StageConnect
	case containsAny(t, "in reply to end of data", "in reply to data", "while sending end of data", "while sending message body"):
		return event.StageData
	case containsAny(t, "in reply to mail from", "while sending mail"):
		return event.StageMail
	case containsAny(t, "in reply to ehlo", "in reply to helo"):
		return event.StageEHLO
	}
	return event.StageRcpt
}

func (p *Parser) when(line string) time.Time {
	if m := isoTime.FindStringSubmatch(line); m != nil {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999-0700", "2006-01-02T15:04:05-0700"} {
			if t, err := time.Parse(layout, m[1]); err == nil {
				return t.UTC()
			}
		}
	}
	if m := syslogTime.FindStringSubmatch(line); m != nil {
		current := p.Now()
		year := p.Year
		if year == 0 {
			year = current.Year()
		}
		t, err := time.ParseInLocation("2006 Jan _2 15:04:05", strconv.Itoa(year)+" "+m[1], p.Location)
		if err != nil && p.Year == 0 {
			t, err = time.ParseInLocation("2006 Jan _2 15:04:05", strconv.Itoa(year-1)+" "+m[1], p.Location)
		}
		if err == nil {
			if p.Year == 0 && t.Sub(current) > 24*time.Hour {
				t = t.AddDate(-1, 0, 0)
			}
			return t.UTC()
		}
	}
	return p.Now()
}

// retry reports whether a delivery line is a retry: the queue id and
// recipient were seen before, or the message waited longer than RetryAfter
// before its last active queue entry.
func (p *Parser) retry(id string, queued float64) bool {
	_, seen := p.seen[id]
	if !seen {
		p.seen[id] = struct{}{}
		p.order = append(p.order, id)
		if len(p.order) > seenIDs {
			delete(p.seen, p.order[0])
			p.order = p.order[1:]
		}
	}
	return seen || queued >= p.RetryAfter.Seconds()
}

func codeFor(dsn, text string) int {
	if m := codeRe.FindStringSubmatch(text); m != nil {
		if c, err := strconv.Atoi(m[1]); err == nil {
			return c
		}
	}
	switch {
	case strings.HasPrefix(dsn, "2"):
		return 250
	case strings.HasPrefix(dsn, "4"):
		return 450
	}
	return 550
}

// connectionStage recognizes a session that failed before the provider
// replied, where the reply code is Postfix's own. A line that carries a
// provider reply never reaches it.
func connectionStage(text string) (event.Stage, bool) {
	t := strings.ToLower(text)
	switch {
	case containsAny(t, "connect to ", "connection refused", "connection timed out", "no route to host", "initial server greeting", "network is unreachable"):
		return event.StageConnect, true
	case strings.Contains(t, "lost connection") && !containsAny(t, "while sending", "after "):
		return event.StageConnect, true
	case containsAny(t, "cannot start tls", "ssl_connect", "tls handshake"):
		return event.StageTLS, true
	case containsAny(t, "after ehlo", "after helo"):
		return event.StageEHLO, true
	}
	return "", false
}

// normalize rewrites an RFC 5424 line into the RFC 3164 shape the patterns
// expect and drops a leading syslog priority.
func normalize(line string) string {
	if m := rfc5424Re.FindStringSubmatch(line); m != nil {
		return m[1] + " " + m[2] + " " + m[3] + "[" + m[4] + "]: " + m[5]
	}
	return priRe.ReplaceAllString(line, "")
}

// bounceKind is policy when the provider refused the sender, by status or
// by reply category, soft for a 4xx status and hard otherwise.
func bounceKind(dsn string, cat classify.Category) event.BounceKind {
	switch {
	case strings.HasPrefix(dsn, "5.7."), cat == classify.ReputationBlock, cat == classify.AuthFailure, cat == classify.Policy:
		return event.Policy
	case strings.HasPrefix(dsn, "4"):
		return event.Soft
	}
	return event.Hard
}
