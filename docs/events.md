# Events

`POST /v1/events` takes one JSON object, a JSON array, or one object per line. It answers 202 with `accepted`, `invalid` and the first ten errors, each with the event's index. It answers 400 when no event was accepted, including an empty array. Each reported error is at most 256 bytes.

A request carries at most 10,000 events; more get 413. A body that is not valid JSON, or has more than 10,000 events, records none of them. The SendGrid webhook has the same 10,000-element limit. The KumoMTA webhook takes at most 10,000 records per request and 10,000 recipients per record, and the records may give at most 10,000 events in total; a request over any of these gets 413 and records nothing.

Every `POST` endpoint, webhooks included, answers 403 to a cross-origin browser request (a `Sec-Fetch-Site` of `cross-site` or `same-site`, or an `Origin` whose host differs from `Host`). MTAs, scripts and ESPs send neither header and pass. A request line and headers over 64 KiB (65,536 bytes) get 431.

```
{"type":"attempt","time":"2026-03-10T08:15:02Z","source":"mx-1","domain":"gmail.com"}
{"type":"reply","time":"2026-03-10T08:15:02Z","source":"mx-1","domain":"gmail.com","code":250,"enhanced":"2.0.0","delay":0.5}
{"type":"reply","time":"2026-03-10T08:15:03Z","source":"mx-1","domain":"yahoo.com","code":421,"enhanced":"4.7.0","text":"[TSS04] Messages from 203.0.113.1 temporarily deferred"}
{"type":"reply","time":"2026-03-10T08:16:00Z","source":"mx-1","domain":"outlook.com","stage":"connect"}
{"type":"complaint","time":"2026-03-10T09:00:00Z","source":"mx-1","domain":"gmail.com"}
```

## Fields on every event

| Field | Required | Meaning |
|-------|----------|---------|
| `type` | yes | One of the types below |
| `time` | yes | RFC 3339. Used only for `ehlo_event_lag_seconds`; warmup days follow the server clock |
| `source` | yes | Sending server or IP pool: 1-64 letters, digits, `.`, `_`, `:` or `-`. At most `max_sources` distinct names (default 256) |
| `v` | | Schema version: 1 |
| `domain` | | Recipient domain. ehlo maps it to a provider such as `gmail` |
| `provider` | | Replaces the domain mapping. An unknown name becomes `other` |
| `route` | | Default `self_hosted`. The SES and SendGrid webhooks set `ses` and `sendgrid`; the KumoMTA webhook keeps `self_hosted`. Other routes must be listed under `routes` in the config. Only `self_hosted` events feed the warmup |
| `count` | | Number of identical events this one stands for. Default 1, max 10,000,000 |
| `ip` | | Accepted and ignored |

## Types

| Type | Fields | Meaning |
|------|--------|---------|
| `attempt` | `attempt`: `first` (default) or `retry`. `signed`: DKIM signed. `engaged`: recipient opened or clicked recently | One delivery attempt. Only first attempts count against the warmup limit |
| `reply` | `code`: 200-299, 400-499 or 500-599; 3xx codes are intermediate replies and are rejected. `enhanced`: such as `4.7.28`. `stage`: `connect`, `ehlo`, `tls`, `mail`, `rcpt` or `data`. `tag`: the provider's token, such as `TSS04` (2-24 of `A-Z 0-9 # : _ ( ) -`); taken from `text` when absent. `text`: up to 512 bytes. `delay`: seconds since the message was queued | The provider's answer. At stage `connect`, `ehlo` or `tls` the session ended before a transaction and `code` is optional |
| `bounce` | `bounce`: `hard`, `soft` or `policy` | A bounce. `soft` is a temporary failure that expired, `policy` a refusal of the sender |
| `complaint` | | A spam complaint |
| `open` | | An open |
| `queue` | `deferred`: messages. `oldest_age`: nanoseconds | A source's deferred queue |
| `dmarc` | `spf`, `dkim`: `pass` or `fail`, both required | One row of a DMARC aggregate report |
| `tls` | `tls_version`: `TLSv1.3`, `TLSv1.2`, `TLSv1.1`, `TLSv1` or `none`. `tls_trust`: `verified`, `trusted`, `untrusted`, `anonymous` or `none` | One outgoing TLS session |

## Postfix

`ehlo tail-postfix` reads Postfix log lines and posts attempts, replies, bounces and TLS sessions.

- A delivery is a retry when the same queue ID and recipient appeared before, or when the message waited in the queue at least `-retry-after` (default 1m).
- `journalctl -o short-iso` lines are read, with the offset written as `+0000` (systemd 252, Debian 12) or `+00:00`.
- Lines from `lmtp`, `local`, `virtual`, `pipe`, `discard`, `error` and `retry` are skipped. None of them talks to a provider.
- Lines whose relay is listed in `-skip-relay` are skipped, so a smarthost's 250 is not counted as the provider's.
- A greeting refusal (`refused to talk to me: 554 ...`) keeps the provider's code and counts as a connection failure.
- RFC 3164 and RFC 5424 lines are read. Lines over 1 MiB are skipped and logged.
- RFC 3164 timestamps carry no zone. `-tz` sets it; the default is the tailer's local zone.
- A batch the server refuses is logged and dropped, not resent.
- While the server is unreachable the tailer keeps at most the newest 20,000 events. Past that it drops the oldest down to 18,000 and logs the dropped count and running total at most once per flush.

## SES and SendGrid

- SES events carry the time of the bounce, complaint, delivery or open, not the send time.
- SES bounces and complaints for addresses SES suppressed (`Suppressed`, `OnAccountSuppressionList`, `OnTenantSuppressionList`, `UnsubscribedRecipient`, `EmailValidationSuppressed`) are skipped. SES made no attempt.
- An SES open is one event. It has a domain only when the message had one recipient.
- SendGrid `deferred` keeps the provider's reply code and text. A `blocked` bounce is soft. `dropped` is skipped.

## KumoMTA

`POST /v1/webhooks/kumomta` takes KumoMTA [log records](https://docs.kumomta.com/reference/log_record/) as a log hook posts them: one JSON object per request (`log_hooks:new_json`), a JSON array (a batched hook) or one record per line. It checks `webhooks.token` and the body limit like the other webhooks. Setup is in [integrations.md](integrations.md#kumomta).

| Record | Events |
|--------|--------|
| `Delivery` | `attempt` and a `reply` with the provider's code, enhanced status and text |
| `TransientFailure` | `attempt` and the `reply` |
| `Bounce` | `attempt`, the `reply` and a `bounce`. Without a session to the provider (a null MX, an MX on `prohibited_hosts`): the `bounce` only |
| `Expiration` | A `soft` bounce, when the record names the source field |
| `OOB` | None: KumoMTA logs it without `egress_source` or `egress_pool` |
| `Feedback` with `feedback_type` `abuse` | A `complaint`, when the source field is in the supplemental trace header |
| Any other type (`Reception`, `Rejection`, `AdminBounce`, `Delayed`, rebinds, transfers) | None |

- `source` is `egress_source`, or `egress_pool` with `?source=egress_pool`. There is no fallback: a record without that field gives no events, so no lane is named after a value the attempts do not use. Names outside the `source` shape are rejected.
- `Feedback` records carry no egress fields. ehlo reads the source field from `feedback_report.supplemental_trace`, the decoded `X-KumoRef` header, which holds the message meta that `trace_headers.include_meta_names` lists. [integrations.md](integrations.md#kumomta) shows the setup. Without it, complaints give no events.
- `Expiration` records never name `egress_source`. They name the queue's `egress_pool` when it is set, and neither when the message expires as the spool loads. They count only with `?source=egress_pool`.
- `route` stays `self_hosted`: KumoMTA is your own MTA, and only `self_hosted` lanes get warmup limits.
- `domain` is the recipient's domain. A record with several recipients gives one event per domain with `count` set. A `Feedback` record takes the domain from the decoded `X-KumoRef` trace header, else from `Original-Rcpt-To`.
- `attempt` is `first` when `num_attempts` is 0 and `retry` otherwise.
- `stage` comes from `response.command`: `EHLO`, `STARTTLS`, `MAIL FROM`, `RCPT TO`, `DATA` or `.`. A reply to no command is the greeting and counts as a connection failure.
- `time` is `event_time`, else `timestamp`. `delay` is the time since `created`.
- Responses KumoMTA writes itself start with `KumoMTA internal:`. `failed to connect to any candidate hosts` is a connection failure, and `failed to send message` (a timeout or a broken session) keeps its code and the stage of its command. Every other internal response (throttles, suspended queues, spool errors) is not an attempt and gives no event.
- `bounce` is `policy` for the classifications `SpamBlock`, `SpamContent`, `SpamRelated`, `PolicyRelated`, `AuthenticationFailed`, `ContentRelated`, `VirusRelated` and `ProhibitedAttachment`, `soft` for `MessageExpired`, and otherwise read from the reply as for Postfix.
- Deliveries by a protocol other than `ESMTP` are skipped, so the log hook's own HTTP deliveries (`Lua`) are not counted.
- No `tls` events: the log record carries `tls_protocol_version` but not whether the certificate was verified, which `tls_trust` needs.
- A body with no event to record, such as only `Reception` records, gets 202 so the log hook does not fail it.

No adapter sends `signed`, `engaged`, queue sizes or DMARC rows, and only the KumoMTA webhook sends complaints from your own feedback loops. Send the rest from your code.
