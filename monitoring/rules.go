package monitoring

import (
	"maps"
	"slices"
	"strings"
)

// RuleGroup is one Prometheus rule group.
type RuleGroup struct {
	Name  string
	Rules []Rule
}

// Rule is a recording rule when Record is set, an alert otherwise.
type Rule struct {
	Record string
	Alert  string
	// Expr is the PromQL with ehlo's own labels only. Rendering adds the
	// selector to every ehlo_ series and the group_by labels to every by
	// and on clause.
	Expr          string
	For           string
	KeepFiringFor string
	Severity      string
	Summary       string
	Description   string
	// Dashboard is the Name of the dashboard that explains the alert.
	Dashboard string
}

// Alert severities.
const (
	critical = "critical"
	warning  = "warning"
	info     = "info"
)

// RecordingFile and AlertsFile are the rule file names.
const (
	RecordingFile = "ehlo-recording.yml"
	AlertsFile    = "ehlo-alerts.yml"
)

// RuleGroups returns the recording group and the alert group. Rendering
// replaces the ehlo at the start of each name with the uid prefix without
// its trailing hyphens.
func RuleGroups() []RuleGroup {
	return []RuleGroup{recordingGroup(), alertGroup()}
}

func recordingGroup() RuleGroup {
	return RuleGroup{Name: "ehlo-recording", Rules: []Rule{
		{Record: "ehlo:lane_quota_used:ratio", Expr: `max by (source, provider) (ehlo_warmup_sent_today_messages)
  / (max by (source, provider) (ehlo_warmup_daily_limit_messages) > 0)`},
		{Record: "ehlo:smtp_deferral:ratio_rate10m", Expr: `sum by (source, provider, code) (rate(ehlo_smtp_replies_total{route="self_hosted", class="4xx"}[10m]))
  / ignoring (code) group_left
sum by (source, provider) (rate(ehlo_smtp_attempts_total{route="self_hosted"}[10m]))`},
		{Record: "ehlo:connect_failure:ratio_rate10m", Expr: `sum by (source, provider) (` + rateOf(`ehlo_smtp_connect_failures_total{route="self_hosted"}`) + `)
  / sum by (source, provider) (` + rateOf(`ehlo_smtp_attempts_total{route="self_hosted"}`) + `)`},
		{Record: "ehlo:complaint:ratio_rate10m", Expr: `sum by (route, source, provider) (` + rateOf(`ehlo_complaints_total`) + `)
  / sum by (route, source, provider) (` + rateOf(`ehlo_delivered_total`) + `)`},
		{Record: "ehlo:bounce:ratio_rate10m", Expr: `sum by (route, kind) (rate(ehlo_bounces_total[10m]))
  / ignoring (kind) group_left
sum by (route) (rate(ehlo_smtp_attempts_total{attempt="first"}[10m]))`},
		{Record: "ehlo:delivery:ratio_rate10m", Expr: `sum by (route) (rate(ehlo_delivered_total[10m]))
  / sum by (route) (rate(ehlo_smtp_attempts_total{attempt="first"}[10m]))`},
		{Record: "ehlo:open:ratio_rate10m", Expr: `sum by (route) (rate(ehlo_opens_total[10m]))
  / sum by (route) (rate(ehlo_delivered_total[10m]))`},
		{Record: "ehlo:dkim_unsigned:ratio_rate10m", Expr: `sum by (source) (` + rateOf(`ehlo_dkim_messages_total{result="unsigned"}`) + `)
  / sum by (source) (` + rateOf(`ehlo_dkim_messages_total{result!="unknown"}`) + `)`},
		{Record: "ehlo:category:ratio_rate10m", Expr: `sum by (source, provider, category) (` + rateOf(`ehlo_reply_categories_total{route="self_hosted", category!="delivered"}`) + `)
  / ignoring (category) group_left
sum by (source, provider) (` + rateOf(`ehlo_smtp_attempts_total{route="self_hosted"}`) + `)`},
		{Record: "ehlo:hard_bounce:ratio_rate10m", Expr: `sum by (route, source, provider) (` + rateOf(`ehlo_bounces_total{kind="hard"}`) + `)
  / sum by (route, source, provider) (` + rateOf(`ehlo_smtp_attempts_total{attempt="first"}`) + `)`},
		{Record: "ehlo:feed_age_seconds", Expr: `time() - max by (route, type) (max_over_time(ehlo_route_last_event_timestamp_seconds[1d]))`},
	}}
}

// telemetryAbsent is the expression of EhloTelemetryAbsent: a target that
// exported ehlo_build_info in the last day and now fails its scrape, or no
// ehlo series at all. With group_by labels, the second term becomes a
// group that exported ehlo series in the last day and has none now, so
// the alert keeps the labels it is routed on.
func telemetryAbsent(groupBy []string) string {
	down := `up == 0 and on (job, instance) present_over_time(ehlo_build_info[1d])`
	gone := `absent(ehlo_build_info)`
	if len(groupBy) > 0 {
		gone = `group by () (present_over_time(ehlo_build_info[1d])) unless on () ehlo_build_info`
	}
	return down + "\nor\n(\n  " + gone + "\n  unless on () (" + down + ")\n)"
}

func alertGroup() RuleGroup {
	return RuleGroup{Name: "ehlo-alerts", Rules: []Rule{
		{Alert: "EhloTelemetryAbsent", Expr: telemetryAbsentMarker, For: "1m", Severity: critical, Dashboard: "pipeline",
			Summary:     "Prometheus cannot scrape the ehlo server",
			Description: "No metrics from ehlo for over 1 minute, so no other ehlo alert can fire. First check that the ehlo process runs and answers on /metrics, then the scrape target in Prometheus."},
		{Alert: "EhloIngestInvalidEvents", For: "5m", Severity: warning, Dashboard: "pipeline",
			Expr: `sum by (type) (rate(ehlo_events_total{result="invalid"}[10m]))
  / sum by (type) (rate(ehlo_events_total[10m])) > 0.01
and on (type)
sum by (type) (increase(ehlo_events_total{result="invalid"}[10m])) >= 5`,
			Summary:     "{{ $value | humanizePercentage }} of {{ $labels.type }} events fail validation",
			Description: "ehlo drops these events, so the dashboards undercount. First read the errors in the POST /v1/events response of the adapter that sends {{ $labels.type }} events."},
		{Alert: "EhloWarmupStateNotSaved", For: "5m", Severity: warning, Dashboard: "pipeline",
			Expr:        `time() - max by () (ehlo_warmup_snapshot_last_success_timestamp_seconds) > 300`,
			Summary:     "ehlo has not saved the warmup state for {{ $value | humanizeDuration }}",
			Description: "A restart now loses the lanes' progress since the last save, or every lane when no save ever succeeded. First check the ehlo log for save warmup snapshot errors and that the state path is writable (on Kubernetes, the /data volume)."},
		{Alert: "EhloLaneAtDailyLimit", Severity: info, Dashboard: "warmup",
			Expr:        `ehlo:lane_quota_used:ratio >= 1`,
			Summary:     "{{ $labels.source }} reached its daily limit toward {{ $labels.provider }}",
			Description: "Expected during warmup. Send the rest of today's mail to {{ $labels.provider }} through another server."},
		{Alert: "EhloProviderCapacityExhausted", For: "2m", Severity: critical, Dashboard: "warmup",
			Expr: `count by (provider) (
    ehlo:lane_quota_used:ratio > 0.9
  or on (source, provider)
    max by (source, provider) (ehlo_warmup_state{state=~"paused|blocked"}) == 1
)
  == count by (provider) (max by (source, provider) (ehlo_warmup_daily_limit_messages))`,
			Summary:     "No server has room left today for mail to {{ $labels.provider }}",
			Description: "Every server toward {{ $labels.provider }} is above 90% of its daily limit, paused or blocked. First send the overflow to {{ $labels.provider }} through an ESP."},
		{Alert: "EhloDKIMUnsigned", For: "1m", Severity: critical, Dashboard: "reputation",
			Expr:        `ehlo:dkim_unsigned:ratio_rate10m > 0`,
			Summary:     "{{ $labels.source }} sends mail without a DKIM signature ({{ $value | humanizePercentage }})",
			Description: "Providers may reject or spam-folder this mail. First route mail away from {{ $labels.source }}, then restart its DKIM signer."},
		{Alert: "EhloProviderRateLimiting", For: "2m", KeepFiringFor: "10m", Severity: warning, Dashboard: "delivery",
			Expr: `ehlo:category:ratio_rate10m{category="rate_limited"} > 0.05
and on (source, provider)
sum by (source, provider) (` + increaseOf(`ehlo_reply_categories_total{route="self_hosted", category="rate_limited"}`, "10m") + `) >= 5`,
			Summary:     "{{ $labels.provider }} slows down {{ $labels.source }} ({{ $value | humanizePercentage }} of attempts)",
			Description: "{{ $labels.provider }} asks {{ $labels.source }} to send less. The warmup engine already pauses the lane for 10 minutes. If this repeats, lower the target of {{ $labels.source }} for {{ $labels.provider }}."},
		{Alert: "EhloAuthenticationRejected", For: "2m", Severity: critical, Dashboard: "reputation",
			Expr:        `sum by (source, provider) (` + increaseOf(`ehlo_reply_categories_total{route="self_hosted", category="auth_failure"}`, "10m") + `) > 0`,
			Summary:     "{{ $labels.provider }} rejects mail from {{ $labels.source }} for failed SPF, DKIM, DMARC, TLS or reverse DNS",
			Description: "{{ $value | humanize }} replies in 10 minutes. First check the DNS records, the DKIM signer and the PTR of the sending IP of {{ $labels.source }}. Do not suppress the recipients."},
		{Alert: "EhloReputationBlock", For: "2m", KeepFiringFor: "10m", Severity: critical, Dashboard: "reputation",
			Expr: `ehlo:category:ratio_rate10m{category="reputation_block"} > 0.01
and on (source, provider)
sum by (source, provider) (` + increaseOf(`ehlo_reply_categories_total{route="self_hosted", category="reputation_block"}`, "10m") + `) >= 5`,
			Summary:     "{{ $labels.provider }} blocks {{ $labels.source }} for reputation ({{ $value | humanizePercentage }} of attempts)",
			Description: "The warmup engine stops the lane until the next day. First request delisting from {{ $labels.provider }} with the URL or code in the reply text. Do not retry 5xx replies."},
		{Alert: "EhloHardBounceRateHigh", For: "10m", Severity: warning, Dashboard: "reputation",
			Expr: `ehlo:hard_bounce:ratio_rate10m{route="self_hosted"} > 0.025
and on (route, source, provider)
sum by (route, source, provider) (` + increaseOf(`ehlo_bounces_total{route="self_hosted", kind="hard"}`, "10m") + `) >= 5`,
			Summary:     "{{ $value | humanizePercentage }} of mail from {{ $labels.source }} to {{ $labels.provider }} goes to addresses that do not exist",
			Description: "A list with many invalid addresses hurts reputation. First find the campaign and suppress each address on its first hard bounce."},
		{Alert: "EhloFeedbackFeedStale", For: "5m", Severity: warning, Dashboard: "pipeline",
			Expr: `ehlo:feed_age_seconds{type=~"complaint|bounce"} > 1800
and on (route) (
    sum by (route) (rate(ehlo_smtp_attempts_total[30m])) > 0
  or
    sum by (route) (rate(ehlo_delivered_total[30m])) > 0
)`,
			Summary:     "No {{ $labels.type }} event on route {{ $labels.route }} for {{ $value | humanizeDuration }} while it keeps sending",
			Description: "A webhook or feedback loop probably stopped. Until it is fixed, the {{ $labels.type }} rate reads zero and the warmup engine counts the day as clean. First check the adapter that sends {{ $labels.type }} events for route {{ $labels.route }}."},
		{Alert: "EhloConnectionFailures", For: "2m", Severity: warning, Dashboard: "delivery",
			Expr: `ehlo:connect_failure:ratio_rate10m > 0.5
and on (source, provider)
sum by (source, provider) (` + increaseOf(`ehlo_smtp_connect_failures_total{route="self_hosted"}`, "10m") + `) >= 5`,
			Summary:     "{{ $labels.source }} cannot connect to {{ $labels.provider }} ({{ $value | humanizePercentage }} of attempts)",
			Description: "Sessions end before any mail is sent. First check whether other servers fail toward {{ $labels.provider }} too: if they do, the provider or the network is the cause; if not, check the IP, PTR and TLS of {{ $labels.source }}."},
		{Alert: "EhloDeferredQueueGrowing", For: "10m", KeepFiringFor: "5m", Severity: warning, Dashboard: "delivery",
			Expr: `max by (source) (ehlo_queue_deferred_messages)
  / on (source) sum by (source) (increase(ehlo_smtp_attempts_total{route="self_hosted", attempt="first"}[1d])) > 0.05
and on (source)
max by (source) (ehlo_queue_deferred_messages) > 1000`,
			Summary:     "{{ $labels.source }} has mail waiting to retry, {{ $value | humanizePercentage }} of a day's volume",
			Description: "A provider defers or refuses mail from {{ $labels.source }}. First open Retry-later codes on the Delivery dashboard to see which replies."},
		{Alert: "EhloQueueFeedStale", For: "5m", Severity: warning, Dashboard: "delivery",
			Expr: `group by (source) (present_over_time(ehlo_queue_deferred_messages[1d]))
  unless on (source) ehlo_queue_deferred_messages
and on (source)
sum by (source) (rate(ehlo_smtp_attempts_total{route="self_hosted"}[30m])) > 0`,
			Summary:     "No queue event from {{ $labels.source }} for over 15 minutes while it keeps sending",
			Description: "ehlo drops the queue size of {{ $labels.source }} 15 minutes after its last queue event, so EhloDeferredQueueGrowing cannot fire for it. First check the job that posts queue events for {{ $labels.source }}."},
		{Alert: "EhloComplaintRateHigh", For: "15m", Severity: critical, Dashboard: "reputation",
			Expr: `sum by (source, provider) (` + increaseOf(`ehlo_complaints_total{route="self_hosted"}`, "24h") + `)
  / sum by (source, provider) (` + increaseOf(`ehlo_delivered_total{route="self_hosted"}`, "24h") + `) > 0.003
and on (source, provider)
sum by (source, provider) (` + increaseOf(`ehlo_complaints_total{route="self_hosted"}`, "24h") + `) >= 5`,
			Summary:     "Recipients at {{ $labels.provider }} report {{ $value | humanizePercentage }} of mail from {{ $labels.source }} as spam",
			Description: "Gmail's published guidance, used here for every provider: stay below 0.1% and never reach 0.3%. If every server rises together, the content or the list is the cause: first stop the campaign."},
		{Alert: "EhloWarmupLanePaused", Severity: info, Dashboard: "warmup",
			Expr:        `max by (source, provider, state) (ehlo_warmup_state{state="paused"}) == 1`,
			Summary:     "Warmup paused {{ $labels.source }} toward {{ $labels.provider }} for spam complaints",
			Description: "The lane sends nothing until it resumes at half its last good limit. Send mail to {{ $labels.provider }} through another server meanwhile."},
		{Alert: "EhloWarmupLaneBlocked", Severity: critical, Dashboard: "warmup",
			Expr:        `max by (source, provider, state) (ehlo_warmup_state{state="blocked"}) == 1`,
			Summary:     "{{ $labels.provider }} blocked {{ $labels.source }}, lane stopped until the next day",
			Description: "Reputation block replies reached 1% of attempts. First request delisting, as in EhloReputationBlock, and send mail to {{ $labels.provider }} through another server."},
		{Alert: "EhloDMARCUnauthenticatedSource", Severity: warning, Dashboard: "reputation",
			Expr:        `sum by (source) (` + increaseOf(`ehlo_dmarc_messages_total{spf="fail", dkim="fail"}`, "1d") + `) > 0`,
			Summary:     "DMARC reports show mail from {{ $labels.source }} that fails both SPF and DKIM",
			Description: "{{ $value | humanize }} messages in the last day. Either a service sends as your domain without SPF or DKIM set up, or someone spoofs the domain. First identify the sending IPs in the report."},
	}}
}

func fresh(sel, window string) string {
	return "(" + sel + " unless " + sel + " offset " + window + " and on (job, instance) ehlo_build_info offset " + window + ")"
}

func increaseOf(sel, window string) string {
	return fresh(sel, window) + " or increase(" + sel + "[" + window + "])"
}

func rateOf(sel string) string {
	return fresh(sel, "10m") + " / 600 or rate(" + sel + "[10m])"
}

// telemetryAbsentMarker stands for the expression telemetryAbsent builds
// from the group_by labels.
const telemetryAbsentMarker = "telemetry-absent"

// ruleExpr applies the selector, the group_by labels and the scope labels
// to a rule expression.
func (r renderer) ruleExpr(expr string) string {
	if expr == telemetryAbsentMarker {
		expr = telemetryAbsent(r.cfg.GroupBy)
	}
	return group(r.rule.inject(expr), r.cfg.keptLabels())
}

// runbookURL is the runbook link of an alert.
func (r renderer) runbookURL(alert string) string {
	return strings.ReplaceAll(r.cfg.RunbookURL, "{{alert}}", strings.ToLower(alert))
}

// ruleLabels are the labels of an alert: the extra labels, component and
// severity.
func (r renderer) ruleLabels(rule Rule) map[string]string {
	labels := maps.Clone(r.cfg.RuleLabels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels["component"] = "ehlo"
	labels["severity"] = rule.Severity
	return labels
}

// ruleGroups renders the groups as YAML-ready maps, in key order, each
// named after the rule namespace.
func (r renderer) ruleGroups(groups []RuleGroup) []any {
	out := make([]any, len(groups))
	for i, g := range groups {
		rules := make([]any, len(g.Rules))
		for k, rule := range g.Rules {
			m := ordered{}
			if rule.Record != "" {
				m = append(m, kv{"record", rule.Record}, kv{"expr", r.ruleExpr(rule.Expr)})
				rules[k] = m
				continue
			}
			m = append(m, kv{"alert", rule.Alert}, kv{"expr", r.ruleExpr(rule.Expr)})
			if rule.For != "" {
				m = append(m, kv{"for", rule.For})
			}
			if rule.KeepFiringFor != "" {
				m = append(m, kv{"keep_firing_for", rule.KeepFiringFor})
			}
			labels := r.ruleLabels(rule)
			lm := ordered{}
			for _, key := range slices.Sorted(maps.Keys(labels)) {
				lm = append(lm, kv{key, labels[key]})
			}
			annotations := ordered{
				kv{"summary", rule.Summary},
				kv{"description", rule.Description},
				kv{"runbook_url", r.runbookURL(rule.Alert)},
				kv{"dashboard_uid", r.uid(rule.Dashboard)},
			}
			if r.cfg.DashboardURL != "" {
				annotations = append(annotations, kv{"dashboard_url", r.dashboardURL(rule.Dashboard)})
			}
			m = append(m, kv{"labels", lm}, kv{"annotations", annotations})
			rules[k] = m
		}
		out[i] = ordered{kv{"name", r.ruleNamespace() + strings.TrimPrefix(g.Name, "ehlo")}, kv{"interval", r.cfg.RuleInterval}, kv{"rules", rules}}
	}
	return out
}
