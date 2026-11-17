package monitoring

import "fmt"

// providerSpec is what differs between the per-provider dashboards: the
// provider label, the complaint lines the provider publishes, and the reply
// codes worth naming in the panel descriptions.
type providerSpec struct {
	name, label, title, about string
	complaintWarn, complaint  float64
	bulkSender                bool
	rateLimit, block, auth    string
}

var providerSpecs = []providerSpec{
	{
		name: "gmail", label: "gmail", title: "Gmail",
		about:         "Gmail and Google Workspace. Gmail asks for a spam rate under 0.1% and never 0.3%, SPF and DKIM and DMARC for senders over 5,000 a day, TLS, and a valid PTR.",
		complaintWarn: 0.001, complaint: 0.003, bulkSender: true,
		rateLimit: "421 4.7.28 means an unusual rate from the IP. Google asks to stop for at least 10 minutes and come back on one connection.",
		block:     "550 5.7.1 likely unsolicited, or 421 4.7.0 low IP reputation.",
		auth:      "421 4.7.26 to 4.7.40 and 550 5.7.26 to 5.7.40: SPF, DKIM, DMARC or alignment failed. 4.7.29 means no TLS.",
	},
	{
		name: "microsoft", label: "outlook", title: "Microsoft (Outlook, Hotmail, Live)",
		about:         "Outlook.com, Hotmail and Live share one infrastructure. Senders over 5,000 a day need SPF, DKIM and DMARC since May 2025. Microsoft allows up to 500 connections at once and publishes no spam rate threshold.",
		complaintWarn: 0.001, complaint: 0.003, bulkSender: true,
		rateLimit: "451 4.7.650 means the IP is rate limited for reputation, common on new IPs. 4.7.652 and RP-001 to RP-003 are connection and rate limits.",
		block:     "550 5.7.1 S3150 or S3140 means the IP is on Microsoft's block list. 5.7.511 is a banned sender.",
		auth:      "550 5.7.515: a sender over 5,000 a day failed SPF, DKIM or DMARC.",
	},
	{
		name: "yahoo", label: "yahoo", title: "Yahoo (Yahoo, AOL)",
		about:         "Yahoo and AOL. Yahoo asks for a spam rate under 0.3%, SPF and DKIM and DMARC for bulk senders, and gradual ramps on new IPs.",
		complaintWarn: 0.001, complaint: 0.003,
		rateLimit: "421 4.7.0 [TSS04] means the IP is temporarily deferred for volume or complaints.",
		block:     "553 5.7.2 [TSS11] means all mail from the IP is permanently deferred.",
		auth:      "DMARC or DKIM failures arrive as 5.7.x replies naming the check.",
	},
}

// providerDashboard is the dashboard fixed to one provider: the same volume
// row as the overview, the provider's health tiles and what it answers.
func providerDashboard(p providerSpec) Dashboard {
	l := fmt.Sprintf(`provider="%s"`, p.label)
	sl := fmt.Sprintf(`provider="%s", source=~"$source"`, p.label)
	lane := self + ", " + sl
	ratio := func(category string) string {
		return fmt.Sprintf(`sum(rate(ehlo_reply_categories_total{%s, category="%s"}[10m])) / sum(rate(ehlo_smtp_attempts_total{%s}[10m]))`, lane, category, lane)
	}
	health := []Panel{
		{Type: "stat", Title: "Complaints", Unit: "none", Decimals: ptr(1),
			Subtitle:    "Per 10,000 delivered, 24h",
			Description: fmt.Sprintf("Recipients who marked an email as spam, per 10,000 delivered, over the last 24 hours. Watch at %.0f, act at %.0f. This is ehlo's count from feedback loops, not the rate %s computes.", p.complaintWarn*10000, p.complaint*10000, p.title),
			Queries:     []Query{{Expr: fmt.Sprintf(`sum(increase(ehlo_complaints_total{%s}[24h])) / (sum(increase(ehlo_delivered_total{%s}[24h])) > 0) * 10000`, lane, lane), Instant: true}},
			Thresholds:  steps("green", at(p.complaintWarn*10000, "yellow"), at(p.complaint*10000, "red"))},
		{Type: "stat", Title: "Slowed down", Unit: "percentunit", Decimals: ptr(2),
			Subtitle:    "Of attempts, last 10 min",
			Description: "Share of attempts in the last 10 minutes that " + p.title + " answered with \"slow down\". " + p.rateLimit,
			Queries:     []Query{{Expr: ratio("rate_limited"), Instant: true}},
			Thresholds:  steps("green", at(0.01, "yellow"), at(0.05, "red"))},
		{Type: "stat", Title: "Blocked", Unit: "percentunit", Decimals: ptr(2),
			Subtitle:    "Of attempts, last 10 min",
			Description: "Share of attempts in the last 10 minutes that " + p.title + " refused because it distrusts the server. " + p.block,
			Queries:     []Query{{Expr: ratio("reputation_block"), Instant: true}},
			Thresholds:  steps("green", at(0.001, "yellow"), at(0.01, "red"))},
		{Type: "stat", Title: "Failed checks", Unit: "short", Decimals: ptr(0),
			Subtitle:    "SPF, DKIM or DMARC, 10 min",
			Description: "Emails refused in the last 10 minutes because " + p.title + " could not confirm they come from your domain (SPF, DKIM or DMARC). Should be 0. " + p.auth,
			Queries:     []Query{{Expr: fmt.Sprintf(`sum(increase(ehlo_reply_categories_total{%s, category="auth_failure"}[10m])) or vector(0)`, lane), Instant: true}},
			Thresholds:  steps("green", at(1, "red"))},
	}
	volume := Panel{Type: "stat", Title: "Sent in the last 24 hours", Unit: "short", Decimals: ptr(0), Neutral: true,
		Subtitle:    "First tries to " + p.title,
		Description: "Emails sent to " + p.title + " in the last 24 hours.",
		Queries:     []Query{{Expr: fmt.Sprintf(`sum(increase(ehlo_smtp_attempts_total{%s, %s, attempt="first"}[1d]))`, self, l), Instant: true}}}
	if p.bulkSender {
		volume.Description += " From 5,000 a day " + p.title + " applies its bulk sender rules: above the line, missing SPF, DKIM or DMARC turns into rejections. The number turns blue above 5,000."
		volume.Neutral = false
		volume.Thresholds = steps("text", at(5000, "blue"))
		volume.Subtitle = "Bulk sender rules apply from 5,000; blue above"
	}
	laneState := laneStatePanel("source", sl)
	laneState.Title = "Warmup status, per server"
	laneState.Subtitle = "What the warmup decided for each server"
	laneState.Description = "What the warmup decided for each server toward " + p.title + ": growing, fully warmed, holding, paused, slowed down or blocked. The Warmup dashboard explains each state."
	return Dashboard{
		Name: p.name, Title: p.title, Description: p.about, Provider: p.label,
		Rows: []Row{
			{Title: "Right now", Groups: []Group{tiles(health...), {Columns: 1, Width: chartWidth, Height: "short", Panels: []Panel{volume}}}},
			{Title: "Sent and delivered", Groups: volumeGroups(lane)},
			{Title: "How " + providerTitle(p.label) + " answers", Groups: []Group{charts(2,
				Panel{Type: "timeseries", Title: "Replies other than accepted, by code", Unit: perMinute,
					Subtitle:    "Click a line for what the code means",
					Description: "Every reply other than accepted, per minute, by code and the provider's own tag. Click a line for what the code means and what to do.",
					Queries:     []Query{{Expr: fmt.Sprintf(`sum by (code) (rate(ehlo_smtp_replies_total{%s, class!="2xx"}%s)) * 60 > 0`, lane, rate), Legend: "{{code}}"}},
					CodeLinks:   true},
				Panel{Type: "timeseries", Title: "Replies other than accepted, by reason", Unit: "percentunit", Stack: "percent",
					Subtitle:    "The same replies, as shares of all of them",
					Description: "The same replies sorted by reason, as a share of all of them: slowed down, blocked, failed checks, unknown address, full mailbox, busy and others.",
					Queries:     []Query{{Expr: fmt.Sprintf(`sum by (category) (rate(ehlo_reply_categories_total{%s, category!="delivered"}%s))`, lane, rate), Legend: "{{category}}"}}},
				Panel{Type: "timeseries", Title: "Slowed down, per server", Unit: "percentunit", ThresholdUI: "line",
					Subtitle:    "At the line, 5%, ehlo pauses for 10 minutes",
					Description: "Share of attempts answered with \"slow down\". At the line, 5%, ehlo stops the server toward " + p.title + " for 10 minutes.",
					Queries:     []Query{{Expr: fmt.Sprintf(`ehlo:category:ratio_rate10m{category="rate_limited", %s}`, sl), Legend: "{{source}}"}},
					Thresholds:  steps("transparent", at(0.05, "orange"))},
				Panel{Type: "timeseries", Title: "Time to deliver", Unit: "s",
					Subtitle:    "Typical and slowest 5%, queue to accepted",
					Description: "Seconds from entering the queue to " + p.title + " accepting the email. typical: half the emails were faster. slowest 5%: 95 of every 100 were faster.",
					Queries: []Query{
						{Expr: fmt.Sprintf(`histogram_quantile(0.50, sum by (le) (rate(ehlo_delivery_seconds_bucket{%s, %s}%s)))`, self, l, rate), Legend: "typical"},
						{Expr: fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(ehlo_delivery_seconds_bucket{%s, %s}%s)))`, self, l, rate), Legend: "slowest 5%"}}},
				Panel{Type: "timeseries", Title: "Failed connections, per server", Unit: "percentunit",
					Subtitle:    "Attempts that never reached a conversation",
					Description: "Share of attempts that failed before " + p.title + " accepted a conversation. Every server failing points at the provider or the network. One server failing points at its IP, reverse DNS or TLS.",
					Queries:     []Query{{Expr: fmt.Sprintf(`ehlo:connect_failure:ratio_rate10m{%s}`, sl), Legend: "{{source}}"}}},
				laneState,
			)}},
			{Title: "Complaints and warmup", Groups: []Group{charts(2,
				Panel{Type: "timeseries", Title: "Spam complaints, per server", Unit: per10k, ThresholdUI: "line",
					Subtitle:    fmt.Sprintf("Per 10,000 delivered; lines at %.0f and %.0f", p.complaintWarn*10000, p.complaint*10000),
					Description: fmt.Sprintf("Complaints per 10,000 delivered, per server. Lines at %.0f and %.0f.", p.complaintWarn*10000, p.complaint*10000),
					Queries:     []Query{{Expr: fmt.Sprintf(`ehlo:complaint:ratio_rate10m{%s, %s} * 10000`, self, sl), Legend: "{{source}}"}},
					Thresholds:  steps("transparent", at(p.complaintWarn*10000, "yellow"), at(p.complaint*10000, "red"))},
				Panel{Type: "bargauge", Title: "Today's sending limit used", Unit: "percentunit", Min: ptr(0.0), Max: ptr(1.0),
					Subtitle:    "Per server; 100% is the day's limit",
					Description: "Each server's share of today's warmup limit toward " + p.title + ". 100% means more mail to " + p.title + " has to go through another server. Expected while a server warms up.",
					Queries:     []Query{{Expr: fmt.Sprintf(`ehlo:lane_quota_used:ratio{%s}`, sl), Legend: "{{source}}", Instant: true}},
					Thresholds:  steps("green", at(1, "blue"))},
			)}},
		},
	}
}
