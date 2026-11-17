# Alerts

One section per alert in `prometheus/rules/ehlo-alerts.yml`, with what fired it and the first thing to do. "Lane" means one source toward one provider ([limits.md](limits.md#terms)).

| Severity | Alerts |
|----------|--------|
| critical | [EhloTelemetryAbsent](#ehlotelemetryabsent), [EhloProviderCapacityExhausted](#ehloprovidercapacityexhausted), [EhloDKIMUnsigned](#ehlodkimunsigned), [EhloAuthenticationRejected](#ehloauthenticationrejected), [EhloReputationBlock](#ehloreputationblock), [EhloComplaintRateHigh](#ehlocomplaintratehigh), [EhloWarmupLaneBlocked](#ehlowarmuplaneblocked) |
| warning | [EhloIngestInvalidEvents](#ehloingestinvalidevents), [EhloProviderRateLimiting](#ehloproviderratelimiting), [EhloHardBounceRateHigh](#ehlohardbounceratehigh), [EhloFeedbackFeedStale](#ehlofeedbackfeedstale), [EhloConnectionFailures](#ehloconnectionfailures), [EhloDeferredQueueGrowing](#ehlodeferredqueuegrowing), [EhloQueueFeedStale](#ehloqueuefeedstale), [EhloDMARCUnauthenticatedSource](#ehlodmarcunauthenticatedsource) |
| info | [EhloLaneAtDailyLimit](#ehlolaneatdailylimit), [EhloWarmupLanePaused](#ehlowarmuplanepaused) |

What every alert carries:

- Labels `component="ehlo"` and `severity`. The dashboards select ehlo's alerts by `component`, so other alerts in a shared Prometheus do not change them.
- Annotations `summary`, `description` with the first step, `runbook_url` pointing to its section here, and `dashboard_uid` of the dashboard that explains it. A render with `dashboard_url` set adds an absolute `dashboard_url` ([monitoring.md](monitoring.md)).

Thresholds on SMTP replies apply to the `self_hosted` route. EhloIngestInvalidEvents, EhloProviderRateLimiting, EhloReputationBlock, EhloHardBounceRateHigh and EhloConnectionFailures also need at least 5 matching events in the last 10 minutes, so one reply on a lane with little traffic does not fire them.

## EhloTelemetryAbsent

Critical. A target that exported `ehlo_build_info` in the last day has failed its scrape for 1 minute, or no target exports ehlo series at all. No other alert can fire until this clears. The alert does not depend on the job name. With `group_by` labels such as `cluster`, the second case becomes a group that exported ehlo series in the last day and exports none now, so the alert keeps the label it is routed on; it resolves a day after the group's last series, for example after a cluster is removed.

Check that the ehlo process runs and answers on `/metrics` (in the demo, `just ps` and `just logs ehlo`), then the scrape target in Prometheus. The warmup state is saved every 30 seconds and on shutdown.

## EhloIngestInvalidEvents

Warning. More than 1% of one event type failed validation over 10 minutes. The `type` label names it. The panels do not count these events.

The `POST /v1/events` response lists the first ten errors with each event's index. Fix the adapter against [events.md](events.md).

## EhloLaneAtDailyLimit

Info. A lane sent its daily limit. This is expected during warmup. `allowed_now` is 0 until the next day, so route that provider's mail through another source.

## EhloProviderCapacityExhausted

Critical. Every lane toward a provider is above 90% of its daily limit, paused or blocked. The rest of the day's mail to that provider has no source.

Check "Warmup status" on the Warmup dashboard for paused or new lanes. Send the overflow through an ESP. A new source takes weeks to warm up, and raising a target costs reputation.

## EhloDKIMUnsigned

Critical. A source sent mail without a DKIM signature. Postfix does this when the signing milter is down and `milter_default_action = accept`. Gmail requires DKIM from senders of 5,000 or more messages a day.

Route away from the source and restart the signer. Set `milter_default_action` to `tempfail` (the default before Postfix 3.11) or `shutdown` (the default from 3.11), so Postfix holds mail while the signer is down.

## EhloProviderRateLimiting

Warning. More than 5% of a lane's attempts got a rate limit reply: Gmail `4.7.28`, Microsoft `4.7.650` or `RP-001`, Yahoo `TSS04`, Orange `OFR..._104`.

The warmup engine stops the lane for 10 minutes, halves its hourly limit for the day, and holds its limit the next day. The alert keeps firing for 10 minutes after the ratio drops, so one backoff gives one notification. If the alert repeats, lower the source's target for that provider.

## EhloAuthenticationRejected

Critical. A provider rejected a source for SPF, DKIM, DMARC, TLS or reverse DNS: Microsoft `5.7.515`, Gmail `5.7.26` or `4.7.29`. The recipients are fine; do not suppress them.

Check the DNS records, the DKIM signer and the PTR of the sending IP. Gmail's reply text names the failed check.

## EhloReputationBlock

Critical. More than 1% of a lane's attempts were blocked for reputation: Microsoft `S3150` or `5.7.511`, Yahoo `TSS11`, Gmail `5.7.28`, Comcast `BL000001`, T-Online `(BL)`. The warmup engine stops the lane until the next day ([EhloWarmupLaneBlocked](#ehlowarmuplaneblocked)). The alert keeps firing for 10 minutes after the ratio drops.

Request delisting: Microsoft sender support, Yahoo Sender Hub, Google Postmaster Tools, or the URL in the reply text. Do not retry 5xx replies.

## EhloHardBounceRateHigh

Warning. More than 2.5% of a lane's first attempts hard bounced over 10 minutes. Amazon SES places accounts under review at 5%.

Find the campaign, clean the list, and suppress an address on its first hard bounce. Addresses that keep bouncing can become spam traps.

## EhloFeedbackFeedStale

Warning. A route keeps sending (attempts or deliveries in the last 30 minutes), but no complaint or bounce event arrived for 30 minutes. A webhook or feedback loop stopped. A route that never received a complaint or bounce event has no last-event time, so this alert cannot fire for it. Until it is fixed, the complaint ratio reads zero and the warmup engine counts the day as clean.

Fix the feed before the day ends.

## EhloConnectionFailures

Warning. More than half of a lane's attempts failed before the SMTP transaction.

If every source fails toward the provider, the cause is the provider or the network. If one source fails, check its IP, PTR and TLS. From that host, `openssl s_client -starttls smtp -connect <mx>:25` shows the failing step.

## EhloDeferredQueueGrowing

Warning. A source's deferred queue holds more than 1,000 messages and more than 5% of its first attempts over the last day. A provider is deferring or refusing its mail.

"Retry-later codes" on the Delivery dashboard shows which replies. [EhloConnectionFailures](#ehloconnectionfailures) or [EhloProviderRateLimiting](#ehloproviderratelimiting) usually fires too.

ehlo drops a source's queue size 15 minutes after its last `queue` event. Send queue events every 1 to 5 minutes. The alert fires after 10 minutes and keeps firing for 5 minutes after the queue size disappears, so a short gap between events does not resolve it. If the events stop, [EhloQueueFeedStale](#ehloqueuefeedstale) takes over.

## EhloQueueFeedStale

Warning. A source reported its queue in the last day, sent attempts in the last 30 minutes, and has had no `queue` event for over 15 minutes. Until the reporter resumes, [EhloDeferredQueueGrowing](#ehlodeferredqueuegrowing) cannot fire for the source. The alert resolves a day after the last queue event.

Check the job that posts queue events for the source.

## EhloComplaintRateHigh

Critical. Over 24 hours, a lane's complaints exceed 0.3% of its deliveries, with at least 5 complaints. The thresholds are Gmail's published guidance, used for every provider: stay below 0.1% and never reach 0.3%. Feedback loops report hours after delivery, hence the 24-hour window.

If every source rises together, the cause is the content or the list: stop the campaign. The warmup engine applies its own pause rule at the end of the day ([limits.md](limits.md#rules)).

## EhloWarmupLanePaused

Info. The warmup engine paused a lane for complaints. It resumes at half its last good limit after `pause_days`.

## EhloWarmupLaneBlocked

Critical. The warmup engine stopped a lane after reputation block replies reached 1% of its attempts. The next day opens at half its last good limit. [EhloReputationBlock](#ehloreputationblock) lists the delisting paths.

## EhloDMARCUnauthenticatedSource

Warning. DMARC reports from the last day show mail from a source that failed both SPF and DKIM. Either a service sends as your domain without SPF or DKIM set up, or someone spoofs the domain.

Set up the service, or confirm the domain publishes `p=reject`.
