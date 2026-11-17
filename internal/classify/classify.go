// Package classify sorts an SMTP reply into the category an operator acts
// on, from its code, enhanced status, provider tag and text.
package classify

import (
	"regexp"
	"strings"

	"github.com/cauebraz/ehlo/internal/event"
)

// Category is what an operator does about a reply.
type Category string

// The categories, from the provider accepting the message to the catch-all.
const (
	Delivered        Category = "delivered"
	Connection       Category = "connection"
	DNS              Category = "dns"
	RateLimited      Category = "rate_limited"
	ReputationBlock  Category = "reputation_block"
	AuthFailure      Category = "auth_failure"
	InvalidRecipient Category = "invalid_recipient"
	MailboxFull      Category = "mailbox_full"
	Content          Category = "content"
	Policy           Category = "policy"
	Busy             Category = "busy"
	Other            Category = "other"
)

// Categories lists every category, Delivered first and Other last.
var Categories = []Category{Delivered, Connection, DNS, RateLimited, ReputationBlock, AuthFailure, InvalidRecipient, MailboxFull, Content, Policy, Busy, Other}

// Reply is the part of an SMTP reply the classifier reads.
type Reply struct {
	Code       int
	Enhanced   string
	Tag        string
	Text       string
	Connection bool
}

type rule struct {
	cat      Category
	enhanced *regexp.Regexp
	tag      *regexp.Regexp
	text     *regexp.Regexp
	// permanent restricts a text rule to 5xx replies, temporary to 4xx.
	permanent bool
	temporary bool
	// fallback rules run last, after every tag and text rule.
	fallback bool
}

func re(s string) *regexp.Regexp { return regexp.MustCompile(s) }

// rules are tried in four passes: specific enhanced codes, provider tags,
// reply text, then broad fallbacks. Within a pass the first match wins, so
// narrower patterns come first.
var rules = []rule{
	// Rate limiting: the provider asks for less, retrying later works.
	{cat: RateLimited, enhanced: re(`^4\.7\.(28|650|651|652)$`)},
	{cat: RateLimited, enhanced: re(`^5\.7\.508$`)},
	{cat: RateLimited, tag: re(`^(TSS?0[124]|RP-00[1-3]|S775|RL0000\d*|AUP#IN-13\d\d|AUP#CXCNCT|AUP#CXMXRT|OFR\d{3}_(104|109|98[89]|99\d)|HL:I[CF]C|MI:(CEL|DMC|SFQ)|LPN\d{3}_4\d\d)$`)},
	{cat: RateLimited, text: re(`(?i)rate limit|unusual rate|too many (connections|messages|emails|recipients|mails|mail)|temporarily deferred due to (unexpected volume|user complaints)|frequency limited|parallel connections|exceeded permitted limits|thread limit exceeded|slow down`)},
	{cat: RateLimited, text: re(`(?i)low reputation`), temporary: true},
	// Authentication: SPF, DKIM, DMARC, TLS or reverse DNS failed.
	{cat: AuthFailure, enhanced: re(`^[45]\.7\.(23|25|26|27|29|30|32|40|506|509|515)$`)},
	{cat: AuthFailure, tag: re(`^(DM\d{6}|ES\d{6}|OFR\d{3}_(107|20\d|39[78]|515|519|535)|LPN\d{3}_401|MI:(SPF|NHD))$`)},
	{cat: AuthFailure, text: re(`(?i)dmarc|dkim|\bspf\b|not authenticated|authentication (failed|required)|does not pass authentication|ptr (record|lookup)|reverse dns|bad helo`)},
	// Reputation: the provider blocked the IP or domain.
	{cat: ReputationBlock, enhanced: re(`^5\.7\.(28|507|511|513|60[6-9]|6[1-4]\d|705|708)$`)},
	{cat: ReputationBlock, tag: re(`^(S3140|S3150|TSS?03|TSS09|TSS11|SC-00[1-4]|DY-00[12]|OU-00[12]|CS0\d|BL\d{6}|AUP#IN-1000|AUP#IN-1370|AUP#CXBL|IPBL\d*|OFR\d{3}_(99|10[0-3]|425|506)|LPN\d{3}_102|HL:(IPB|IHU)|DT:RBL|GU_EIB_\d+|SFR_IN_\d+|\(BL\))$`)},
	{cat: ReputationBlock, text: re(`(?i)block ?list|blacklist|spamhaus|dnsbl|\brbl\b|unsolicited|likely spam|suspected spam|banned|permanently deferred|spam message rejected|too many spams|\(bl\)|low reputation|blocked for user complaints`), permanent: true},
	// Recipient: the address does not exist or refuses the sender.
	{cat: InvalidRecipient, enhanced: re(`^5\.(1\.[1-9][0-9]*|2\.1|4\.1)$`)},
	{cat: InvalidRecipient, tag: re(`^OFR\d{3}_416$`)},
	{cat: InvalidRecipient, text: re(`(?i)(no such|unknown|invalid|does not exist|not found).{0,20}(user|recipient|mailbox|address)|user unknown|mailbox unavailable|recipient (address )?rejected|not our customer|policy rejection on the target address`)},
	// Mailbox: full.
	{cat: MailboxFull, enhanced: re(`^[45]\.2\.2$`)},
	{cat: MailboxFull, text: re(`(?i)mailbox (is )?full|over quota|quota exceeded|storage`)},
	// DNS: the recipient domain does not resolve.
	{cat: DNS, enhanced: re(`^[45]\.4\.[34]$`)},
	{cat: DNS, text: re(`(?i)host or domain name not found|name service error|nxdomain|unresolvable|\bdns (lookup|error|failure)`)},
	// Connection: the session ended before a transaction.
	{cat: Connection, enhanced: re(`^4\.4\.[1267]$`)},
	// Content: the message itself.
	{cat: Content, enhanced: re(`^5\.6\.[0-9]+$`)},
	{cat: Content, tag: re(`^(DT:SPM|LPN\d{3}_518)$`)},
	{cat: Content, text: re(`(?i)content|attachment|virus|message size|too large|spammy`)},
	// Busy: try again later, nothing about the sender.
	{cat: Busy, enhanced: re(`^4\.(2\.1|3\.[0-9]+|5\.3|7\.500|7\.8[5-9]\d)$`)},
	{cat: Busy, tag: re(`^GL\d{2}$`)},
	{cat: Busy, text: re(`(?i)server busy|service (not |un)available|greylist|resources temporarily unavailable|try again later|too many errors|temporarily unavailable`)},
	// Fallbacks by status class.
	{cat: ReputationBlock, text: re(`(?i)reputation`), fallback: true},
	{cat: Policy, enhanced: re(`^[45]\.7\.[0-9]+$`), fallback: true},
}

// Classify returns the category of a reply.
func Classify(r Reply) Category {
	if r.Connection && r.Code == 0 {
		return Connection
	}
	if r.Code >= 200 && r.Code < 300 {
		return Delivered
	}
	tag := strings.ToUpper(r.Tag)
	for _, rl := range rules {
		if !rl.fallback && rl.enhanced != nil && r.Enhanced != "" && rl.enhanced.MatchString(r.Enhanced) {
			return rl.cat
		}
	}
	for _, rl := range rules {
		if rl.tag != nil && tag != "" && rl.tag.MatchString(tag) {
			return rl.cat
		}
	}
	for _, rl := range rules {
		if rl.fallback || rl.text == nil || r.Text == "" {
			continue
		}
		if (rl.permanent && r.Code < 500) || (rl.temporary && (r.Code < 400 || r.Code >= 500)) {
			continue
		}
		if rl.text.MatchString(r.Text) {
			return rl.cat
		}
	}
	for _, rl := range rules {
		if !rl.fallback {
			continue
		}
		if rl.text != nil && r.Text != "" && rl.text.MatchString(r.Text) {
			return rl.cat
		}
		if rl.enhanced != nil && r.Enhanced != "" && rl.enhanced.MatchString(r.Enhanced) {
			return rl.cat
		}
	}
	return Other
}

// Event classifies a reply event.
func Event(e *event.Event) Category {
	return Classify(Reply{Code: e.Code, Enhanced: e.Enhanced, Tag: e.Tag, Text: e.Text, Connection: e.Stage.ConnectionLevel()})
}

// tagPatterns are the tokens providers put in their reply texts. Each
// captures one token in group 1. The first match wins.
var tagPatterns = []*regexp.Regexp{
	re(`\[(TSS?\d{2}|GL\d{2})\]`),
	re(`\b(RP-\d{3}|SC-\d{3}|DY-\d{3}|OU-\d{3})\b`),
	re(`\((S\d{3,5})\)`),
	re(`\b(S3140|S3150|S775)\b`),
	re(`\[(CS\d{2})\]`),
	re(`\b((?:RL|BL|DM|ES)\d{6})\b`),
	re(`(AUP#(?:In-\d{4}|CX[A-Z]+))`),
	re(`\b(OFR\d{3}_\d{3})\b`),
	re(`\b(LPN\d{3}_\d{3})\b`),
	re(`\b((?:HL|MI|DT):[A-Z]{3})\b`),
	re(`\b(GU_EIB_\d+|SFR_IN_\d+)\b`),
	re(`(\(BL\))`),
}

// Tag extracts the provider's own token from a reply text: Yahoo's
// [TSS04], Microsoft's (S3150) or RP-001, Comcast's BL000001, Orange's
// OFR004_104. Text with no known format, or a token longer than
// event.ValidTag accepts, returns the empty string, so the label set stays
// bounded and the event stays valid.
func Tag(text string) string {
	for _, p := range tagPatterns {
		if m := p.FindStringSubmatch(text); m != nil {
			if tag := strings.ToUpper(m[1]); event.ValidTag(tag) {
				return tag
			}
		}
	}
	return ""
}

// KnownCodes are the reply code labels, as event.CodeLabelWithTag builds
// them, that providers document and the Grafana reply-code catalog
// explains. The metrics keep these labels past their cardinality bounds.
var KnownCodes = []string{
	"421 4.7.28",
	"421 4.7.0",
	"421 4.7.29",
	"450 4.2.1",
	"452 4.2.2",
	"550 5.1.1",
	"550 5.7.1",
	"550 5.7.26",
	"550 5.7.28",
	"451 4.7.650 S775",
	"421 RP-001",
	"550 5.7.1 S3150",
	"550 5.7.515",
	"421 4.7.0 TSS04",
	"553 5.7.2 TSS11",
}
