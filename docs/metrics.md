# Metrics

`GET /metrics` serves the series below. Labels are `route`, `source` and `provider` unless the table says otherwise.

| Metric | Labels |
|--------|--------|
| `ehlo_smtp_attempts_total` | + `attempt` (`first`, `retry`) |
| `ehlo_smtp_replies_total` | + `class` (`2xx`, `4xx`, `5xx`, `connect`), `code` (such as `421 4.7.0 TSS04`; bounded, see [When series appear](#when-series-appear)) |
| `ehlo_reply_categories_total` | + `category`, see below |
| `ehlo_smtp_connect_failures_total` | |
| `ehlo_delivered_total` | |
| `ehlo_delivery_seconds` | `route`, `provider`. Histogram of queue-to-delivery time |
| `ehlo_bounces_total` | + `kind` (`hard`, `soft`, `policy`) |
| `ehlo_complaints_total` | |
| `ehlo_opens_total` | |
| `ehlo_audience_messages_total` | + `audience` (`engaged`, `all`, `unknown`) |
| `ehlo_dkim_messages_total` | `route`, `source`, `result` (`signed`, `unsigned`, `unknown`) |
| `ehlo_smtp_tls_sessions_total` | `route`, `source`, `version`, `trust` |
| `ehlo_dmarc_messages_total` | `source`, `spf`, `dkim` |
| `ehlo_queue_deferred_messages` | `source` |
| `ehlo_queue_oldest_age_seconds` | `source` |
| `ehlo_route_last_event_timestamp_seconds` | `route`, `type`. Receipt time of the last event |
| `ehlo_event_lag_seconds` | `type`. Histogram of receipt time minus event time |
| `ehlo_events_total` | `type` (an event type, or `unknown` for an invalid event whose type is not one), `result` (`accepted`, `invalid`) |
| `ehlo_build_info` | `version` |

Replaying an old log with `tail-postfix -file` fills the top bucket of `ehlo_event_lag_seconds`.

## Warmup

Labels `source` and `provider`, read from the lanes at scrape time. See [limits.md](limits.md).

`ehlo_warmup_daily_limit_messages`, `ehlo_warmup_hourly_limit_messages`, `ehlo_warmup_sent_today_messages`, `ehlo_warmup_delivered_today_messages`, `ehlo_warmup_bounces_today`, `ehlo_warmup_complaints_today`, `ehlo_warmup_target_messages`, `ehlo_warmup_share_ratio` (the lane's limit over all limits toward the provider), `ehlo_warmup_day`, `ehlo_warmup_backoffs`, and `ehlo_warmup_state` with a `state` label set to 1 for the current state.

## Recording rules

`prometheus/rules/ehlo-recording.yml` defines `ehlo:lane_quota_used:ratio`, `ehlo:feed_age_seconds` and these 10-minute ratios: `delivery`, `complaint`, `bounce`, `hard_bounce`, `open`, `smtp_deferral`, `connect_failure`, `category`, `dkim_unsigned`, each named `ehlo:<name>:ratio_rate10m`. `ehlo:lane_quota_used:ratio` has no sample for a lane whose daily limit is 0, such as a paused lane. Every recording rule aggregates by ehlo's own labels, such as `source` and `provider`, plus the `group_by` labels and the labels of the selector's equality matchers of the render ([monitoring.md](monitoring.md)), so its series carry no `instance`, and no `job` unless the selector sets one. Gauges are taken once per source or lane (`max by`), so two ehlo processes reporting the same lane do not double it.

## When series appear

For each name under `sources` in the config, these series start at zero on route `self_hosted`: attempts, connect failures, deliveries, delivery time, bounces, complaints, opens, audience, categories, DKIM, TLS, DMARC and `ehlo_events_total`. A missing series then means an adapter stopped, not a quiet day.

Replies, queue gauges, last-event times and lag appear with the first event. Warmup series appear when the lane exists.

`ehlo_queue_deferred_messages` and `ehlo_queue_oldest_age_seconds` for a source disappear 15 minutes after its last `queue` event and come back with the next one. A queue reporter that stops leaves no stale value behind, so send `queue` events more often than every 15 minutes.

Clients cannot create unbounded label values: an unknown `provider` becomes `other`, a `route` must be configured, `max_sources` caps `source`, and an invalid event with an unknown `type` counts as `unknown`.

`ehlo_smtp_replies_total` bounds its series, not only its `code` values. A lane is one `route`, `source` and `provider`.

- Always kept, in every lane: the documented provider codes in `classify.KnownCodes` (`internal/classify/classify.go`), the stages `connect`, `ehlo` and `tls`, and the class fallbacks `2xx`, `4xx` and `5xx`. At most 21 series per lane.
- A code with an enhanced status or a tag, such as `451 4.7.1`: at most 64 per lane.
- All other series together, those codes and bare codes such as `451`: at most 4,096 across all lanes.
- A reply past the lane bound counts under its bare code, such as `451`. Past the 4,096 bound it counts under its class, such as `code="4xx"`. A code already in use keeps counting.

## Reply categories

ehlo tries, in order: the enhanced status code, the provider's tag, the reply text, then two fallbacks (text mentioning reputation, then any `x.7.x` code). The first match wins. A 2xx is always `delivered`.

A tag is a provider token taken from the text when the event has none: `[TSS04]`, `(S3150)`, `BL000001`, `OFR004_104`, `AUP#In-1310`, `HL:ICC`. Text in no known format gives no tag, and so does a token longer than 24 characters.

| Category | Examples | Meaning |
|----------|----------|---------|
| `delivered` | 250 | Accepted |
| `rate_limited` | Gmail `4.7.28`, Microsoft `4.7.650` and `RP-001`, Yahoo `TSS04`, Comcast `RL000001`, Orange `OFR..._104`, NetEase `HL:ICC` | Slow down |
| `reputation_block` | Microsoft `S3150`, `5.7.507` and `5.7.511`, Yahoo `TSS11`, Gmail `5.7.28`, Comcast `BL000001`, T-Online `(BL)`, "block list", Spamhaus or "unusual rate" in a 5xx | IP or domain blocked |
| `auth_failure` | Microsoft `5.7.515` and `5.7.509`, Gmail `5.7.26` and `4.7.29`, Comcast `DM000001`, Orange `OFR..._515` | SPF, DKIM, DMARC, TLS or PTR failed |
| `invalid_recipient` | `5.1.1` | Address does not exist |
| `mailbox_full` | `4.2.2` | Mailbox over quota |
| `connection` | No reply at stage `connect`, `ehlo` or `tls`; `4.4.1` | Session never started |
| `dns` | `4.4.3` | Domain does not resolve |
| `content` | `5.6.x`, "message size" | Problem with the message |
| `busy` | `4.2.1`, `4.3.x`, `4.7.500`, greylisting, "try again later" | Retry later. Not about the sender |
| `policy` | Other `x.7.x`, such as Microsoft `5.7.510` | Policy reply with no narrower match |
| `other` | Anything else | |

The rules are in `internal/classify/classify.go`.
