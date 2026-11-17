package monitoring

import (
	"strconv"
	"strings"
)

// Dashboard is one Grafana dashboard. Its uid is Config.UIDPrefix plus
// Name, and its file is the uid plus .json.
type Dashboard struct {
	Name        string
	Title       string
	Description string
	// Provider fixes the dashboard to one provider and drops the provider
	// variable.
	Provider string
	// Range is the default time range, now-3h when empty.
	Range string
	Rows  []Row
}

// Row is one Grafana row and the groups of panels under it. A collapsed row
// holds its panels until a reader opens it.
type Row struct {
	Title     string
	Groups    []Group
	Collapsed bool
}

// Group is a run of panels in one responsive grid. The grid fits as many
// columns of at least Width pixels as the screen allows, up to Columns,
// wraps the rest, and gives every panel the Height preset: short (168
// pixels), standard (320) or tall (512).
type Group struct {
	Columns int
	Width   int
	Height  string
	Panels  []Panel
}

// Minimum column widths in pixels. A tile is a stat; a chart is anything
// with an axis, a list or text. Panel titles and subtitles must fit them.
const (
	tileWidth  = 200
	chartWidth = 360
	textWidth  = 448
)

// tiles is a row of up to four stat tiles.
func tiles(ps ...Panel) Group {
	return Group{Columns: 4, Width: tileWidth, Height: "short", Panels: ps}
}

// charts is a grid of charts, at most columns side by side.
func charts(columns int, ps ...Panel) Group {
	return Group{Columns: columns, Width: chartWidth, Height: "standard", Panels: ps}
}

// tall returns g with the tall row height, for charts with one line per
// lane.
func (g Group) tall() Group {
	g.Height = "tall"
	return g
}

const (
	src        = `source=~"$source"`
	prov       = `provider=~"$provider"`
	both       = `source=~"$source", provider=~"$provider"`
	self       = `route="self_hosted"`
	rate       = "[$__rate_interval]"
	ehloAlerts = `component="ehlo"`
)

// Dashboards returns every dashboard, in navigation order.
func Dashboards() []Dashboard {
	ds := []Dashboard{
		overviewDashboard(),
		warmupDashboard(),
		deliveryDashboard(),
		reputationDashboard(),
		pipelineDashboard(),
	}
	for _, p := range providerSpecs {
		ds = append(ds, providerDashboard(p))
	}
	return append(ds, codesDashboard())
}

// Units a non-specialist reads without converting: emails per minute and
// complaints per 10,000 delivered.
const (
	perMinute = "suffix: /min"
	per10k    = "suffix: per 10,000"
)

func overviewDashboard() Dashboard {
	return Dashboard{Name: "overview", Title: "Overview", Description: "Is email working right now, and how much was sent and delivered? Each number opens the dashboard that explains it.", Rows: []Row{
		{Title: "Is email working?", Groups: []Group{tiles(headline()...)}},
		{Title: "Needs attention", Groups: []Group{tiles(attention()...), charts(1, alertsTable())}},
		{Title: "Sent and delivered", Groups: volumeGroups(self)},
		{Title: "By provider", Groups: []Group{charts(2,
			Panel{Type: "bargauge", Title: "Delivery rate by provider", Unit: "percentunit", Min: ptr(0.9), Max: ptr(1.0), Decimals: ptr(2),
				Subtitle:    "Accepted share in the time range",
				Description: "Share of emails each provider accepted in the selected time range. Healthy: 99% or more. Watch: 97% to 99%. Act: under 97%.",
				Queries:     []Query{{Expr: `sum by (provider) (increase(ehlo_delivered_total{` + self + `}[$__range])) / (sum by (provider) (increase(ehlo_smtp_attempts_total{` + self + `, attempt="first"}[$__range])) > 0)`, Legend: "{{provider}}", Instant: true}},
				Options:     obj{"displayMode": "basic"},
				Thresholds:  steps("red", at(0.97, "yellow"), at(0.99, "green"))},
			Panel{Type: "bargauge", Title: "Emails sent by provider", Unit: "short", Decimals: ptr(0), Neutral: true,
				Subtitle:    "First tries in the time range",
				Description: "Emails sent to each provider in the selected time range, first tries only.",
				Queries:     []Query{{Expr: `sum by (provider) (increase(ehlo_smtp_attempts_total{` + self + `, attempt="first"}[$__range])) > 0`, Legend: "{{provider}}", Instant: true}},
				Options:     obj{"displayMode": "basic", "valueMode": "text"}},
		)}},
	}}
}

func warmupDashboard() Dashboard {
	state := laneStatePanel("source, provider", both)
	state.Title = "Warmup status"
	state.Subtitle = "What the warmup decided, per server and provider"
	state.Description = "One line per server and provider. Growing: the daily limit rises after each clean day. Fully warmed: the server sends its whole target. Holding: the limit stays put after complaints, bounces, low volume or a slowdown. Paused: complaints were too high, no sending for a day. Slowed down: the provider asked to slow down, sending stops for a few minutes. Blocked: the provider blocked the server, sending stops until tomorrow."
	return Dashboard{Name: "warmup", Title: "Warmup", Range: "now-7d", Description: "How much each server may send to each provider today, and why. A new server starts small and grows each clean day.", Rows: []Row{
		{Title: "Today", Groups: []Group{charts(2,
			Panel{Type: "bargauge", Title: "Today's sending limit used", Unit: "percentunit", Min: ptr(0.0), Max: ptr(1.0),
				Subtitle:    "Per server and provider; 100% is the day's limit",
				Description: "One bar per server and provider (a lane). 100% means the server sent everything ehlo allowed today toward that provider, and more mail to it has to go through another server. Expected while a server warms up.",
				Queries:     []Query{{Expr: `ehlo:lane_quota_used:ratio{` + both + `}`, Legend: "{{source}} to {{provider}}", Instant: true}},
				Thresholds:  steps("green", at(1, "blue"))},
			state,
		).tall()}},
		{Title: "During the day", Groups: []Group{charts(3,
			Panel{Type: "timeseries", Title: "Hourly allowance", Unit: "short",
				Subtitle:    "Emails each server may send this hour",
				Description: "Emails each warming server may send per hour toward each provider. It halves for the rest of the day after the provider asks to slow down. A fully warmed server has no hourly cap and shows its daily limit until a slow-down reply; then it gets the warming cap, halved.",
				Queries:     []Query{{Expr: `max by (source, provider) (ehlo_warmup_hourly_limit_messages{` + both + `})`, Legend: "{{source}} to {{provider}}"}}},
			Panel{Type: "timeseries", Title: "Slowdowns today", Unit: "short",
				Subtitle:    "Each one stops tomorrow's growth",
				Description: "Times today the provider asked a server to slow down or blocked it. Each one stops tomorrow's growth for that server and provider.",
				Queries:     []Query{{Expr: `max by (source, provider) (ehlo_warmup_backoffs{` + both + `})`, Legend: "{{source}} to {{provider}}"}}},
			Panel{Type: "timeseries", Title: "Spam complaints today", Unit: per10k, ThresholdUI: "line",
				Subtitle:    "Growth stops at 10, sending pauses at 30",
				Description: "Complaints per 10,000 delivered today, as the warmup counts them. At 10 the limit stops growing, at 30 the server pauses toward that provider. The end-of-day decision also uses the previous days.",
				Queries:     []Query{{Expr: `max by (source, provider) (ehlo_warmup_complaints_today{` + both + `}) / (max by (source, provider) (ehlo_warmup_delivered_today_messages{` + both + `}) > 0) * 10000`, Legend: "{{source}} to {{provider}}"}},
				Thresholds:  steps("transparent", at(10, "yellow"), at(30, "red"))},
		)}},
		{Title: "Ramp", Groups: []Group{charts(3,
			Panel{Type: "timeseries", Title: "Daily limit growth", Unit: "short", LogScale: true,
				Subtitle:    "Log scale: a steady ramp is a straight line",
				Description: "Each server's daily limit toward each provider, on a log scale so a ramp from 50 to 100,000 reads as a straight line. Each provider has its own growth pace.",
				Queries:     []Query{{Expr: `max by (source, provider) (ehlo_warmup_daily_limit_messages{` + both + `}) > 0`, Legend: "{{source}} to {{provider}}"}}},
			Panel{Type: "timeseries", Title: "Share of provider volume", Unit: "percentunit", Min: ptr(0.0), Max: ptr(1.0),
				Subtitle:    "Each server's part of the provider's limits",
				Description: "Each server's limit as a share of all limits toward the provider. A router that splits mail by this share fills every server at the same pace.",
				Queries:     []Query{{Expr: `max by (source, provider) (ehlo_warmup_share_ratio{` + both + `})`, Legend: "{{source}} to {{provider}}"}}},
			Panel{Type: "timeseries", Title: "Mail to engaged readers", Unit: "percentunit", Min: ptr(0.0), Max: ptr(1.0),
				Subtitle:    "Recipients who opened or clicked recently",
				Description: "Share of emails sent to recipients who opened or clicked recently. A warming server should send mostly to them: they rarely complain, which builds reputation.",
				Queries: []Query{{Legend: "{{source}}", Expr: `sum by (source) (rate(ehlo_audience_messages_total{audience="engaged", ` + self + `, ` + src + `}` + rate + `))
/ sum by (source) (rate(ehlo_audience_messages_total{` + self + `, ` + src + `}` + rate + `))`}}},
		)}},
	}}
}

func deliveryDashboard() Dashboard {
	return Dashboard{Name: "delivery", Title: "Delivery", Description: "What the providers answer, how long delivery takes, and what waits to be retried.", Rows: []Row{
		{Title: "Provider replies", Groups: []Group{
			charts(2,
				Panel{Type: "timeseries", Title: "Why emails were not delivered", Unit: perMinute, Stack: "normal",
					Subtitle:    "Replies other than accepted, by reason",
					Description: "Every reply other than accepted, per minute, sorted by reason: slowed down (rate_limited), blocked (reputation_block), failed checks (auth_failure), unknown address (invalid_recipient), full mailbox, busy, and others. The Reply codes dashboard explains each code.",
					Queries:     []Query{{Expr: `sum by (category) (rate(ehlo_reply_categories_total{` + self + `, ` + both + `, category!="delivered"}` + rate + `)) * 60`, Legend: "{{category}}"}}},
				Panel{Type: "timeseries", Title: "Time to deliver, by provider", Unit: "s",
					Subtitle:    "Typical and slowest 5%, queue to accepted",
					Description: "Seconds from entering the queue to the provider accepting the email. typical: half the emails were faster. slowest 5%: 95 of every 100 were faster. A provider whose slow line climbs alone is deferring mail.",
					Queries: []Query{
						{Expr: `histogram_quantile(0.50, sum by (provider, le) (rate(ehlo_delivery_seconds_bucket{` + self + `, ` + prov + `}` + rate + `)))`, Legend: "{{provider}} typical"},
						{Expr: `histogram_quantile(0.95, sum by (provider, le) (rate(ehlo_delivery_seconds_bucket{` + self + `, ` + prov + `}` + rate + `)))`, Legend: "{{provider}} slowest 5%"}}},
			),
			charts(3,
				Panel{Type: "timeseries", Title: "Slowed down by providers", Unit: "percentunit", ThresholdUI: "line",
					Subtitle:    "At the line, 5%, ehlo pauses for 10 minutes",
					Description: "Share of attempts the provider answered with \"slow down\": Gmail 421 4.7.28, Microsoft 451 4.7.650 and RP-001, Yahoo TSS04. At the line, 5%, ehlo stops the server toward that provider for 10 minutes.",
					Queries:     []Query{{Expr: `ehlo:category:ratio_rate10m{category="rate_limited", ` + both + `}`, Legend: "{{source}} to {{provider}}"}},
					Thresholds:  steps("transparent", at(0.05, "orange"))},
				Panel{Type: "timeseries", Title: "Failed connections", Unit: "percentunit",
					Subtitle:    "Attempts that never reached a conversation",
					Description: "Share of attempts that failed before the provider accepted a conversation: connection refused or timed out, greeting refused, or TLS failed. Every server failing points at the provider or the network; one server failing points at its IP, reverse DNS or TLS.",
					Queries:     []Query{{Expr: `ehlo:connect_failure:ratio_rate10m{` + both + `} > 0`, Legend: "{{source}} to {{provider}}"}}},
				Panel{Type: "timeseries", Title: "Retry-later codes", Unit: "percentunit",
					Subtitle:    "Click a line for what the code means",
					Description: "Share of attempts answered with a temporary error (4xx), by reply code. 421 4.7.28 is Gmail's unusual rate reply, 452 4.2.2 a full mailbox. Click a line for what the code means.",
					Queries: []Query{{Legend: "{{code}}", Expr: `sum by (route, code) (rate(ehlo_smtp_replies_total{` + self + `, class="4xx", ` + both + `}` + rate + `))
			/ ignoring (code) group_left sum by (route) (rate(ehlo_smtp_attempts_total{` + self + `, ` + both + `}` + rate + `))`}},
					CodeLinks: true},
			),
			charts(1,
				Panel{Type: "table", Title: "Most retry-later replies", Unit: "percentunit",
					Subtitle:    "Top 10 server and provider pairs, last 10 minutes",
					Description: "The 10 server and provider pairs with the highest share of temporary errors over the last 10 minutes.",
					Queries:     []Query{{Expr: `topk(10, sum by (source, provider) (ehlo:smtp_deferral:ratio_rate10m{` + both + `}))`, Instant: true, Legend: "{{source}} to {{provider}}"}},
					Transformations: []Transform{
						{"labelsToFields", obj{"mode": "columns"}},
						{"merge", obj{}},
						{"organize", obj{
							"excludeByName": obj{"Time": true},
							"indexByName":   obj{"source": 0, "provider": 1, "Value": 2},
							"renameByName":  obj{"Value": "retry-later share", "source": "server"},
						}},
						{"sortBy", obj{"sort": []any{obj{"field": "retry-later share", "desc": true}}}},
					}},
			),
		}},
		{Title: "Waiting to retry", Groups: []Group{charts(3,
			Panel{Type: "timeseries", Title: "Waiting, per server", Unit: "short",
				Subtitle:    "Emails held for a later retry",
				Description: "Emails each server holds for a later retry (its deferred queue). Steps follow the server's retry schedule.",
				Queries:     []Query{{Expr: `max by (source) (ehlo_queue_deferred_messages{` + src + `})`, Legend: "{{source}}"}}},
			Panel{Type: "timeseries", Title: "Oldest waiting email", Unit: "s",
				Subtitle:    "Age of the oldest email in each queue",
				Description: "How long the oldest waiting email has been in each server's queue. It grows until the email is delivered or the server gives up and returns it to the sender.",
				Queries:     []Query{{Expr: `max by (source) (ehlo_queue_oldest_age_seconds{` + src + `})`, Legend: "{{source}}"}}},
			Panel{Type: "timeseries", Title: "First tries and retries", Unit: perMinute, Stack: "normal",
				Subtitle:    "A retry wave after each queue run is normal",
				Description: "Emails tried for the first time and retried, per minute. A retry wave after each queue run is normal. Retries climbing while first tries hold means a provider keeps deferring.",
				Queries:     []Query{{Expr: `sum by (attempt) (rate(ehlo_smtp_attempts_total{` + self + `, ` + both + `}` + rate + `)) * 60`, Legend: "{{attempt}}"}}},
		)}},
		{Title: "Encryption (TLS)", Collapsed: true, Groups: []Group{charts(1,
			Panel{Type: "timeseries", Title: "Encrypted connections", Unit: perMinute, Stack: "normal",
				Subtitle:    "By TLS version; none means unencrypted",
				Description: "Connections opened per minute, by TLS version and certificate check. none means the email went unencrypted. Gmail asks for TLS and shows an encryption rate in Postmaster Tools.",
				Queries:     []Query{{Expr: `sum by (version, trust) (rate(ehlo_smtp_tls_sessions_total{` + self + `, ` + src + `}` + rate + `)) * 60`, Legend: "{{version}} {{trust}}"}}},
		)}},
	}}
}

func reputationDashboard() Dashboard {
	return Dashboard{Name: "reputation", Title: "Reputation", Description: "How recipients and providers judge the mail: spam complaints, bounces, blocks and sender checks.", Rows: []Row{
		{Title: "Complaints and bounces", Groups: []Group{charts(3,
			Panel{Type: "timeseries", Title: "Complaints by provider", Unit: per10k, ThresholdUI: "line",
				Subtitle:    "Per 10,000 delivered; stay under 10, never 30",
				Description: "Recipients who marked an email as spam, per 10,000 delivered. Lines at 10 and 30: Gmail asks to stay under 10 and never reach 30, and the warmup holds and pauses at the same values.",
				Queries:     []Query{{Expr: `sum by (provider) (rate(ehlo_complaints_total{` + self + `, ` + both + `}` + rate + `)) / sum by (provider) (rate(ehlo_delivered_total{` + self + `, ` + both + `}` + rate + `)) * 10000`, Legend: "{{provider}}"}},
				Thresholds:  steps("transparent", at(10, "yellow"), at(30, "red"))},
			Panel{Type: "timeseries", Title: "Complaints per server", Unit: "suffix: /hour",
				Subtitle:    "One server rising alone points at its mail",
				Description: "Complaints per hour for each server. One server rising alone points at what it carried. All of them rising points at the content or the list.",
				Queries:     []Query{{Expr: `sum by (source) (increase(ehlo_complaints_total{` + self + `, ` + both + `}[1h]))`, Legend: "{{source}}"}}},
			Panel{Type: "timeseries", Title: "Bounces by kind", Unit: "percentunit",
				Subtitle:    "Share of emails sent: hard, soft, policy",
				Description: "Bounces as a share of emails sent. hard: the address does not exist; remove it from the list. soft: a temporary problem that lasted too long. policy: the provider refused the sender, not the address.",
				Queries: []Query{{Legend: "{{kind}}", Expr: `sum by (route, kind) (rate(ehlo_bounces_total{` + self + `, ` + both + `}` + rate + `))
			/ ignoring (kind) group_left sum by (route) (rate(ehlo_smtp_attempts_total{` + self + `, ` + both + `, attempt="first"}` + rate + `))`}}},
		)}},
		{Title: "Blocks and sender checks", Groups: []Group{charts(2,
			Panel{Type: "timeseries", Title: "Rejected: reputation or sender checks", Unit: perMinute,
				Subtitle:    "Blocked reputation or failed sender checks",
				Description: "Emails refused per minute because the provider distrusts the server (reputation_block: Microsoft S3150, Yahoo TSS11, 5.7.511) or could not confirm the email comes from your domain (auth_failure: SPF, DKIM or DMARC; Outlook 5.7.515, Gmail 5.7.26). Both are about the sender, never the recipient.",
				Queries:     []Query{{Expr: `sum by (provider, category) (rate(ehlo_reply_categories_total{` + self + `, ` + both + `, category=~"auth_failure|reputation_block"}` + rate + `)) * 60`, Legend: "{{provider}} {{category}}"}}},
			Panel{Type: "timeseries", Title: "Emails without a signature (DKIM)", Unit: "percentunit",
				Subtitle:    "Unsigned share; should be 0",
				Description: "Share of emails sent without a DKIM signature, the check providers use to confirm the email really comes from your domain. Should be 0. Above 0, the signing service is down or skipped.",
				Queries:     []Query{{Expr: `ehlo:dkim_unsigned:ratio_rate10m{` + src + `}`, Legend: "{{source}}"}}},
		)}},
		{Title: "DMARC reports", Collapsed: true, Groups: []Group{charts(2,
			Panel{Type: "timeseries", Title: "DMARC: failed checks per server", Unit: "short", Stack: "normal",
				Subtitle:    "Per sender, from the providers' daily reports",
				Description: "Emails that providers reported, in their daily DMARC reports, as failing SPF or DKIM. Your own server with its signer down fails DKIM only. A sender that fails both is either a service nobody set up or someone using your domain.",
				Queries: []Query{
					{Expr: `sum by (source) (increase(ehlo_dmarc_messages_total{dkim="fail"}[1d]))`, Legend: "{{source}} DKIM failed"},
					{Expr: `sum by (source) (increase(ehlo_dmarc_messages_total{spf="fail"}[1d]))`, Legend: "{{source}} SPF failed"}}},
			Panel{Type: "timeseries", Title: "DMARC: reported emails per server", Unit: "short", Stack: "normal",
				Subtitle:    "Per sender; an unknown sender needs a look",
				Description: "Emails providers reported per sender in their daily DMARC reports. A sender that is not one of yours is a service nobody registered or someone using your domain.",
				Queries:     []Query{{Expr: `sum by (source) (increase(ehlo_dmarc_messages_total[1d]))`, Legend: "{{source}}"}}},
		)}},
		{Title: "Own servers against email services", Groups: []Group{charts(3,
			Panel{Type: "timeseries", Title: "Delivery rate", Unit: "percentunit",
				Subtitle:    "self_hosted against ses and sendgrid",
				Description: "Each line is one way of sending: self_hosted is your own servers, ses and sendgrid are email services that carry part of the mail as a comparison.",
				Queries:     []Query{{Expr: `ehlo:delivery:ratio_rate10m`, Legend: "{{route}}"}}},
			Panel{Type: "timeseries", Title: "Bounce rate", Unit: "percentunit",
				Subtitle:    "Hard and soft bounces, per way of sending",
				Description: "Hard and soft bounces as a share of emails sent, for each way of sending.",
				Queries:     []Query{{Expr: `sum by (route) (ehlo:bounce:ratio_rate10m{kind=~"hard|soft"})`, Legend: "{{route}}"}}},
			Panel{Type: "timeseries", Title: "Open rate", Unit: "percentunit",
				Subtitle:    "Opens as a share of delivered emails",
				Description: "Opens as a share of delivered emails. It tells whether your own servers keep the engagement an email service gets.",
				Queries:     []Query{{Expr: `ehlo:open:ratio_rate10m`, Legend: "{{route}}"}}},
		)}},
	}}
}

func pipelineDashboard() Dashboard {
	return Dashboard{Name: "pipeline", Title: "Pipeline", Description: "Are events still arriving? A silent feed looks like a clean day on every other dashboard.", Rows: []Row{
		{Title: "Incoming events", Groups: []Group{
			tiles(Panel{Type: "stat", Title: "Rejected events", Unit: "short", Decimals: ptr(0),
				Subtitle:    "Malformed, last 10 minutes",
				Description: "Events ehlo refused in the last 10 minutes because they were malformed. Each one is a gap in the other dashboards. Should be 0.",
				Queries:     []Query{{Expr: `sum(increase(ehlo_events_total{result="invalid"}[10m])) or vector(0)`, Instant: true}},
				Thresholds:  steps("green", at(1, "red"))}),
			charts(2,
				Panel{Type: "timeseries", Title: "Events received by type", Unit: perMinute, Stack: "normal",
					Subtitle:    "A type that drops to zero is a stopped feed",
					Description: "Events accepted per minute, by type. A type that drops to zero while email keeps going out is a feed that stopped.",
					Queries:     []Query{{Expr: `sum by (type) (rate(ehlo_events_total{result="accepted"}` + rate + `)) * 60`, Legend: "{{type}}"}}},
				Panel{Type: "bargauge", Title: "Time since last event", Unit: "s", Min: ptr(0.0), Max: ptr(3600.0),
					Subtitle:    "Past 30 minutes the feed has probably stopped",
					Description: "Time since the last complaint, bounce and open event for each way of sending. Past 30 minutes while email keeps going out, the feed has probably stopped, and ehlo would read the silence as a clean day.",
					Queries:     []Query{{Expr: `time() - max by (route, type) (ehlo_route_last_event_timestamp_seconds{type=~"complaint|bounce|open"})`, Legend: "{{route}} {{type}}", Instant: true}},
					Thresholds:  steps("green", at(1800, "yellow"))},
			),
		}},
	}}
}

// headline is the top row of the overview: one status in words and the
// three numbers a non-specialist asks about. Each tile links to the
// dashboard that explains it.
func headline() []Panel {
	return []Panel{
		{Type: "stat", Title: "Email health",
			Subtitle:    "From the open alerts",
			Description: "Healthy: no alert is open. Watch: a warning is open; look at Needs attention below within the hour. Act now: a critical alert is open and the on-call operator should act. docs/alerts.md explains each alert.",
			Queries:     []Query{{Expr: `max((ALERTS{` + ehloAlerts + `, alertstate="firing", severity="critical"} * 2) or ALERTS{` + ehloAlerts + `, alertstate="firing", severity="warning"} or vector(0))`, Instant: true}},
			Mappings:    []Mapping{{0, "Healthy", "green"}, {1, "Watch", "yellow"}, {2, "Act now", "red"}},
			Options:     obj{"colorMode": "background", "textMode": "value"}},
		{Type: "stat", Link: "delivery", Title: "Delivered now", Unit: "percentunit", Decimals: ptr(2),
			Subtitle:    "Accepted, last 10 minutes",
			Description: "Share of emails the providers accepted in the last 10 minutes. Healthy: 99% or more. Watch: 97% to 99%. Act: under 97%. An email a provider asked to retry later counts as not delivered until the retry succeeds.",
			Queries:     []Query{{Expr: `ehlo:delivery:ratio_rate10m{` + self + `}`, Instant: true}},
			Thresholds:  steps("red", at(0.97, "yellow"), at(0.99, "green"))},
		{Type: "stat", Link: "reputation", Title: "Complaints", Unit: "none", Decimals: ptr(1),
			Subtitle:    "Per 10,000 delivered, 24h",
			Description: "Recipients who marked an email as spam, per 10,000 delivered, over the last 24 hours. Gmail asks senders to stay under 10 and never reach 30. Complaints arrive hours after delivery, hence the 24-hour window.",
			Queries:     []Query{{Expr: `sum(increase(ehlo_complaints_total{` + self + `}[24h])) / (sum(increase(ehlo_delivered_total{` + self + `}[24h])) > 0) * 10000`, Instant: true}},
			Thresholds:  steps("green", at(10, "yellow"), at(30, "red"))},
		{Type: "stat", Link: "delivery", Title: "Time to deliver", Unit: "s", Decimals: ptr(1),
			Subtitle:    "95 of 100 emails, 10 min",
			Description: "95 of every 100 emails were accepted within this time after entering the queue, over the last 10 minutes. Healthy: under a minute. Retries make it longer.",
			Queries:     []Query{{Expr: `histogram_quantile(0.95, sum by (le) (rate(ehlo_delivery_seconds_bucket{` + self + `}[10m])))`, Instant: true}},
			Thresholds:  steps("green", at(60, "yellow"), at(600, "red"))},
	}
}

// attention is the overview row for the on-call operator: the signals
// that explain a Watch or Act now, each linked to its dashboard.
func attention() []Panel {
	return []Panel{
		{Type: "stat", Link: "delivery", Title: "Connect errors", Unit: "percentunit", Decimals: ptr(2),
			Subtitle:    "Of attempts, last 10 min",
			Description: "Share of attempts in the last 10 minutes where the server could not open a conversation with the provider. Under 5% is normal. Above 50% the provider or the network is unreachable.",
			Queries:     []Query{{Expr: `sum(rate(ehlo_smtp_connect_failures_total{` + self + `}[10m])) / (sum(rate(ehlo_smtp_attempts_total{` + self + `}[10m])) > 0)`, Instant: true}},
			Thresholds:  steps("green", at(0.05, "yellow"), at(0.5, "red"))},
		{Type: "stat", Link: "delivery", Title: "Awaiting retry", Unit: "short", Neutral: true,
			Subtitle:    "Deferred, all servers",
			Description: "Emails a provider asked to send again later (the deferred queue), summed over all servers. A few thousand is normal at volume. A number that keeps growing means a provider is refusing mail.",
			Queries:     []Query{{Expr: `sum(max by (source) (ehlo_queue_deferred_messages))`, Instant: true}}},
		{Type: "stat", Link: "warmup", Title: "Paused senders", Unit: "short",
			Subtitle:    "Server and provider pairs",
			Description: "Server and provider pairs that ehlo stopped for now: the provider asked to slow down, blocked the server, or complaints were too high. Their mail goes through other servers until they resume.",
			Queries:     []Query{{Expr: `sum(max by (source, provider) (ehlo_warmup_state{state=~"backoff|blocked|paused"})) or vector(0)`, Instant: true}},
			Thresholds:  steps("green", at(1, "yellow"))},
		{Type: "stat", Link: "reputation", Title: "Unsigned mail", Unit: "short", Decimals: ptr(0),
			Subtitle:    "No DKIM, last 10 min",
			Description: "Emails sent in the last 10 minutes without a DKIM signature, the check providers use to confirm an email really comes from your domain. Should be 0.",
			Queries:     []Query{{Expr: `sum(increase(ehlo_dkim_messages_total{result="unsigned"}[10m])) or vector(0)`, Instant: true}},
			Thresholds:  steps("green", at(1, "red"))},
	}
}

// alertsTable lists open alerts with their labels as columns, and hides
// while none is open.
func alertsTable() Panel {
	return Panel{Type: "table", Title: "Open alerts", HideEmpty: true,
		Subtitle:    "Firing now; docs/alerts.md explains each",
		Description: "Alerts open now. critical: act now. warning: act within the hour. info: for awareness. docs/alerts.md explains each one and what to do. The table hides while no alert is open.",
		Queries:     []Query{{Expr: `ALERTS{` + ehloAlerts + `, alertstate="firing"}`, Instant: true}},
		Options:     obj{"sortBy": []any{obj{"displayName": "severity", "desc": false}}},
		Transformations: []Transform{
			{"labelsToFields", obj{"mode": "columns"}},
			{"merge", obj{}},
			{"organize", obj{
				"excludeByName": obj{"Time": true, "Value": true, "ALERTS": true, "GRAFANA_ALERTS": true, "alertstate": true, "grafana_alertstate": true, "grafana_rule_uid": true, "component": true, "__name__": true},
				"indexByName":   obj{"alertname": 0, "severity": 1, "source": 2, "provider": 3, "code": 4},
				"renameByName":  obj{"alertname": "alert", "source": "server"},
			}},
		}}
}

// volumeGroups is the sent, delivered, bounced and delivery rate tiles and
// the per-minute chart for a selector, over the dashboard's time range.
func volumeGroups(sel string) []Group {
	sent := `sum(increase(ehlo_smtp_attempts_total{` + sel + `, attempt="first"}[$__range]))`
	delivered := `sum(increase(ehlo_delivered_total{` + sel + `}[$__range]))`
	return []Group{
		tiles(
			Panel{Type: "stat", Title: "Sent", Unit: "short", Decimals: ptr(0), Neutral: true,
				Subtitle:    "First tries, time range",
				Description: "Emails handed to the providers in the selected time range. Retries of the same email are not counted again.",
				Queries:     []Query{{Expr: sent, Instant: true}}},
			Panel{Type: "stat", Title: "Delivered", Unit: "short", Decimals: ptr(0), Neutral: true,
				Subtitle:    "Accepted, time range",
				Description: "Emails the providers accepted in the selected time range, including retries that succeeded. Accepted does not tell inbox from spam folder.",
				Queries:     []Query{{Expr: delivered, Instant: true}}},
			Panel{Type: "stat", Title: "Bounced", Unit: "short", Decimals: ptr(0), Neutral: true,
				Subtitle:    "Returned, time range",
				Description: "Emails returned as undeliverable in the selected time range: unknown addresses, temporary problems that lasted too long, and refusals of the sender.",
				Queries:     []Query{{Expr: `sum(increase(ehlo_bounces_total{` + sel + `}[$__range]))`, Instant: true}}},
			Panel{Type: "stat", Title: "Delivery rate", Unit: "percentunit", Decimals: ptr(2),
				Subtitle:    "Delivered of sent",
				Description: "Delivered as a share of sent in the selected time range. Healthy: 99% or more. Watch: 97% to 99%. Act: under 97%.",
				Queries:     []Query{{Expr: delivered + ` / (` + sent + ` > 0)`, Instant: true}},
				Thresholds:  steps("red", at(0.97, "yellow"), at(0.99, "green"))},
		),
		charts(1, Panel{Type: "timeseries", Title: "Emails per minute", Unit: perMinute,
			Subtitle:    "Sent, delivered and bounced, per minute",
			Description: "Emails sent, delivered and bounced per minute. A gap between sent and delivered is mail being retried later, bounced or refused.",
			Queries: []Query{
				{Expr: `sum(rate(ehlo_smtp_attempts_total{` + sel + `, attempt="first"}` + rate + `)) * 60`, Legend: "sent"},
				{Expr: `sum(rate(ehlo_delivered_total{` + sel + `}` + rate + `)) * 60`, Legend: "delivered"},
				{Expr: `sum(rate(ehlo_bounces_total{` + sel + `}` + rate + `)) * 60`, Legend: "bounced"},
			}}),
	}
}

// laneStatePanel is the state timeline of the warmup lanes grouped by the
// labels in by and filtered by selector. The caller sets Title, Subtitle
// and Description.
func laneStatePanel(by, selector string) Panel {
	labels := strings.Split(by, ", ")
	legend := make([]string, len(labels))
	for k, l := range labels {
		legend[k] = "{{" + l + "}}"
	}
	states := []string{"raising", "holding", "paused", "backoff", "blocked"}
	terms := make([]string, len(states))
	for k, s := range states {
		term := `max by (` + by + `) (ehlo_warmup_state{state="` + s + `", ` + selector + `})`
		if k > 0 {
			term = strconv.Itoa(k+1) + " * " + term
		}
		terms[k] = term
	}
	return Panel{Type: "state-timeline",
		Queries:  []Query{{Legend: strings.Join(legend, " to "), Expr: strings.Join(terms, "\n+ ")}},
		Mappings: []Mapping{{0, "Fully warmed", "green"}, {1, "Growing", "blue"}, {2, "Holding", "yellow"}, {3, "Paused", "red"}, {4, "Slowed down", "orange"}, {5, "Blocked", "dark-red"}},
	}
}
