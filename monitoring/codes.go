package monitoring

import (
	"fmt"
	"strings"
)

// codeNote explains one SMTP reply as the provider documents it. The
// Grafana "Reply codes" dashboard shows one text panel per note, and the
// reply-code charts link each series to its panel.
type codeNote struct {
	Code     string // the series label, code plus enhanced status plus tag
	Provider string
	Meaning  string
	Says     string // what the provider's own page says
	Action   string
	Category string
}

// codeNotes is the catalog. Sources: Gmail SMTP error page, Outlook.com
// postmaster troubleshooting and high-volume sender requirements, Yahoo
// SMTP error codes.
var codeNotes = []codeNote{
	{"421 4.7.28", "gmail", "Gmail sees an unusual rate of mail from the IP, the netblock or the domain and defers.",
		"Gmail has detected an unusual rate of email. To protect our users from spam, email has been temporarily rate limited. Review the Email sender guidelines.",
		"ehlo already stops the lane for 10 minutes and halves its hourly allowance. If it repeats daily, the warmup ramp is too steep for this IP: lower growth.gmail or the source's target.", "rate_limited"},
	{"421 4.7.0", "gmail", "A temporary refusal. The text says which: low IP reputation, a missing PTR, or a plain try again later.",
		"Retry later. For the PTR variant, publish a reverse DNS record whose forward lookup returns the IP.",
		"Read the reply text in the Delivery dashboard. Low reputation at volume means the lane grew faster than the reputation did.", "busy"},
	{"421 4.7.29", "gmail", "Gmail rate limits the mail because the connection has no TLS.",
		"Use a TLS connection for every message.",
		"Enable opportunistic or mandatory TLS on the Postfix transport (smtp_tls_security_level = may or encrypt) and check the Encrypted connections panel on the Delivery dashboard.", "auth_failure"},
	{"450 4.2.1", "gmail", "One recipient receives mail faster than Gmail allows. This is about the mailbox, not the IP.",
		"Resend later.",
		"No lane action. A burst to one address, often a shared inbox or a list.", "busy"},
	{"452 4.2.2", "gmail", "The mailbox is over quota.",
		"Resend later.",
		"No lane action. Postfix retries until maximal_queue_lifetime, then returns a soft bounce.", "mailbox_full"},
	{"550 5.1.1", "gmail", "The address does not exist.",
		"Do not resend.",
		"Suppress the address in your list on the first occurrence. A list with many of these is heading for spam traps.", "invalid_recipient"},
	{"550 5.7.1", "gmail", "Three meanings share this code: a recipient or domain policy refused the message, Gmail judged it likely unsolicited, or Gmail blocked the IP for an unusual rate of unsolicited mail.",
		"For the policy variant, nothing to do on your side. For the unsolicited and unusual rate variants, review the content, the list and the Gmail sender guidelines.",
		"Check the reply text. ehlo classifies the unsolicited and unusual rate variants as reputation_block and stops the lane for the day.", "policy"},
	{"550 5.7.26", "gmail", "The message was not authenticated: neither SPF nor DKIM passed.",
		"Set up SPF or DKIM for the sending domain. Bulk senders need both.",
		"Check the DKIM signer and the SPF record of the Return-Path domain. Nothing about the recipients changes this.", "auth_failure"},
	{"550 5.7.28", "gmail", "Gmail blocks the IP for an unusual rate of unsolicited mail. The permanent twin of 421 4.7.28.",
		"Review sending practices and the Gmail sender guidelines.",
		"ehlo stops the lane for the day. Check Postmaster Tools for the IP and domain reputation before resuming.", "reputation_block"},
	{"451 4.7.650 S775", "outlook", "Microsoft rate limits the IP for reputation. Common on new IPs and after a volume jump.",
		"Keep retrying later. Check the IP in SNDS.",
		"The lane backs off. If the IP is new, the ramp toward Microsoft is too fast: growth.outlook defaults to 1.3 for this reason.", "rate_limited"},
	{"421 RP-001", "outlook", "Microsoft limits the IP's rate because of its IP or domain reputation.",
		"Improve the reputation of the sending IP and domain. Check the IP in SNDS.",
		"Lower shaping.outlook.connections and per_connection, and let the backoff run.", "rate_limited"},
	{"550 5.7.1 S3150", "outlook", "The IP, or part of its netblock, is on Microsoft's block list.",
		"Open a ticket with Outlook.com sender support and check SNDS.",
		"ehlo stops the lane for the day. Delisting takes a ticket; sending more does not help.", "reputation_block"},
	{"550 5.7.515", "outlook", "A sender over 5,000 messages a day failed SPF, DKIM or DMARC. Microsoft rejects these messages from May 5, 2025.",
		"Pass SPF and DKIM and publish a DMARC record aligned with the From domain.",
		"Check the three records for the domain. The Sent in the last 24 hours tile on the Microsoft dashboard shows whether you are above the line.", "auth_failure"},
	{"421 4.7.0 TSS04", "yahoo", "Yahoo defers the IP for unexpected volume or user complaints.",
		"Reduce volume, ramp gradually, and open a Sender Support request if it persists.",
		"The lane backs off. A new IP toward Yahoo ramps at 1.35x by default; if TSS04 repeats, lower it.", "rate_limited"},
	{"553 5.7.2 TSS11", "yahoo", "All mail from the IP is permanently deferred.",
		"Open a Yahoo Sender Support request.",
		"ehlo stops the lane for the day. Nothing you send clears this; the support request does.", "reputation_block"},
}

const codesName = "codes"

// codePanelID is the id of a code's text panel, stable across generations.
func codePanelID(i int) int { return 1000 + i }

// codesDashboard is one text panel per catalogued reply, grouped by
// category, with ids the by-code charts link to.
func codesDashboard() Dashboard {
	byCategory := map[string][]int{}
	var order []string
	for i, n := range codeNotes {
		if _, seen := byCategory[n.Category]; !seen {
			order = append(order, n.Category)
		}
		byCategory[n.Category] = append(byCategory[n.Category], i)
	}
	var rows []Row
	for _, cat := range order {
		var panels []Panel
		for _, i := range byCategory[cat] {
			n := codeNotes[i]
			panels = append(panels, Panel{
				ID: codePanelID(i), Type: "text", Title: n.Code + " (" + n.Provider + ")",
				Description: n.Meaning,
				Options: obj{"mode": "markdown", "content": fmt.Sprintf(
					"**What it means.** %s\n\n**What %s says.** %s\n\n**What to do.** %s\n\nCategory: `%s`. Back: use the browser back button or the dashboard list above.",
					n.Meaning, providerTitle(n.Provider), n.Says, n.Action, n.Category)},
			})
		}
		rows = append(rows, Row{Title: strings.ReplaceAll(cat, "_", " "), Groups: []Group{
			{Columns: 2, Width: textWidth, Height: "standard", Panels: panels},
		}})
	}
	return Dashboard{
		Name: codesName, Title: "Reply codes", Range: "now-3h",
		Description: "What each SMTP reply means, what the provider says about it, and what to do. The reply-code charts link here.",
		Rows:        rows,
	}
}

// providerTitle is the company name used in the panel text.
func providerTitle(p string) string {
	switch p {
	case "gmail":
		return "Google"
	case "outlook":
		return "Microsoft"
	case "yahoo":
		return "Yahoo"
	}
	return p
}

// codeLinks returns field overrides that give each catalogued series on a
// by-code chart a data link to its text panel, and every other series a
// link to the dashboard.
func (r renderer) codeLinks() []fieldOverride {
	out := make([]fieldOverride, len(codeNotes))
	for i, n := range codeNotes {
		out[i] = fieldOverride{
			Matcher: matcher{ID: "byName", Options: n.Code},
			Properties: []linkProperty{{ID: "links", Value: []dataLink{{
				Title: "What " + n.Code + " means and what to do",
				URL:   fmt.Sprintf("%s?viewPanel=%s&${__url_time_range}&${__all_variables}", r.dashboardPath(codesName), elementName(codePanelID(i))),
			}}}},
		}
	}
	return out
}
