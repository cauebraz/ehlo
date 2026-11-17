# Limits

ehlo keeps one lane per source and provider. Each lane has a daily limit that grows toward a target. ehlo advises: it does not send, queue or refuse mail. The code that picks a source for a batch asks `GET /v1/limits` and stays within the answer.

## Terms

| Term | Meaning |
|------|---------|
| Source | A sending server or IP pool, named in each event's `source`, such as `mx-1` |
| Provider | A mailbox provider such as `gmail`, `outlook` or `yahoo`. ehlo maps the recipient domain to it; unknown domains map to `other` |
| Lane | One source toward one provider. Limits, backoffs and pauses apply per lane |
| Route | How mail leaves: `self_hosted` (your MTA), `ses`, `sendgrid`, or a name under `routes`. Only `self_hosted` events feed the lanes; ESP routes get metrics and alerts only |
| Target | The daily volume a lane grows toward: the source's target (`targets.sources.<name>.daily`, else `targets.default`) times the provider's share in `targets.mix`. A provider not in `mix` gets the whole unlisted remainder |
| Limit | Messages the lane may send today, counted in first attempts. Retries do not count |
| Warm source | A source listed in `targets.warm`. Its lanes start at their target |

With `targets.default: 200000` and `mix: {gmail: 0.55, outlook: 0.20, yahoo: 0.10}`, every lane toward Gmail has a target of 110,000, and every lane toward another provider, such as `icloud`, 30,000. A source with no target is unmanaged: it gets metrics and alerts, and `/v1/limits` answers 404 with `managed: false`.

## Rules

| Condition | Effect |
|-----------|--------|
| New lane | Starts at `start_limit` (50). Starts at its target when the source is warm or the target is not above 50 |
| Clean day, at least 70% of the limit sent | Limit grows: Gmail x1.5, Yahoo x1.35, Microsoft x1.3, others x1.4 |
| Complaints reach 0.1% of deliveries, or bounces 3% of first attempts | Limit holds |
| Complaints reach 0.3% | Lane pauses for one day, then resumes at half its last good limit |
| Rate limit replies reach 5% of attempts in 5 minutes | Lane stops for 10 minutes. Its hourly limit halves for the rest of the day |
| Reputation block replies reach 1% of attempts in 5 minutes | Lane stops until the next day, which opens at half its last good limit |
| No first attempt for 30 days | Limit returns to `start_limit` |

A resumed limit is never below `start_limit` or above the target. Ratios cover the last three closed days, so late feedback still counts. A pause empties that window when it ends, so the days that caused it do not pause the lane again. Below 50 deliveries in that window, daily counts replace the complaint ratios: 2 complaints hold, 3 pause. The bounce ratio applies from 50 first attempts, so a new lane that bounces most of its first 50 messages holds. The backoff rules need at least 20 attempts and 5 matching replies in the window.

With every day clean and fully used, a lane goes from 50 to 100,000 a day in 19 days toward Gmail, 23 toward most providers, 26 toward Yahoo and 29 toward Microsoft.

## Days and hours

- A day starts at `decision_hour` UTC on the server clock. Event timestamps never move it. With `day_length` under 24h, days are `day_length` slots from the Unix epoch and `decision_hour` is ignored.
- An hour is `day_length / 24`, counted from the day start.
- Below its target, a lane may send `limit / hours_per_day` per hour. The default 16 allows 1.5x the flat rate.
- At its target, a lane has no hourly limit until a backoff. From then until the end of the day, its hourly limit is `limit / hours_per_day`, halved once per backoff, as below the target.

## API

`GET /v1/limits/{source}/{provider}` returns one lane. `GET /v1/limits` returns `{"lanes": [...], "server_time": ...}`, filtered by `?source=` and `?provider=`. The list holds only lanes that exist: a lane appears after its first event, or after its first `GET /v1/limits/{source}/{provider}` when the source is in the config.

```
{
  "source": "mx-1", "provider": "gmail", "managed": true,
  "state": "raising", "reason": "clean day, limit x1.50",
  "allowed_now": 4.69, "limit": 75, "sent": 40, "remaining": 35,
  "target": 100000, "day": 1,
  "hourly_limit": 4.69, "sent_hour": 0, "remaining_hour": 4.69,
  "next_hour_at": "2026-03-10T09:00:00Z",
  "delivered_today": 39, "bounces_today": 0, "complaints_today": 0,
  "engaged_only": true, "backoffs": 0,
  "shaping": {"connections": 5, "per_connection": 50},
  "day_start": "2026-03-10T00:00:00Z", "day_end": "2026-03-11T00:00:00Z",
  "server_time": "2026-03-10T08:12:01Z"
}
```

- Send up to `allowed_now`, rounded down, then ask again. It is the smaller of the daily and hourly remainders, and 0 while the lane is stopped.
- While a lane is paused, backed off or blocked, `resume_at` and the `Retry-After` header say when to ask again. When a limit is used up, ask again at `next_hour_at` or `day_end`.
- While `engaged_only` is true, send only to recipients who opened or clicked recently.
- `shaping` is advice for the MTA: parallel connections and messages per connection toward the provider. In Postfix, set `<transport>_destination_concurrency_limit` on a transport per provider.
- A 404 with `managed: false` means the source has no target, the provider is unknown, or the source has neither a config entry nor a first event.

The endpoint creates a lane for a source listed in `sources`, `targets.sources` or `targets.warm`. Any other source gets a lane with its first event, so a typo in a router does not create one.

### From a sender

Before each batch, for each provider in the batch:

1. Ask every candidate source, one call each: `GET /v1/limits/{source}/gmail`. Do not use `GET /v1/limits?provider=gmail` here: it lists only lanes that already exist, so a new source is missing from it and never gets traffic.
2. Give each source at most `floor(allowed_now)` messages. A source with 0 gets none.
3. Put what no source can take back in the queue, or send it through an ESP route.
4. Send, and let the adapters report the attempts. `sent` grows as the attempt events arrive, so ask again for the next batch instead of keeping a local count.

The demo simulator (`internal/sim`) routes its traffic this way: it asks `GET /v1/limits/{source}/{provider}` for each of its sources, splits each provider's volume across sources in proportion to their limits, capped by `allowed_now`, and spreads the overflow over the sources with room left.

A lane that just started on a new source has little room. On its first day toward Gmail, `limit` is 50 and the hourly limit is 50 / 16 = 3.125, so the lane may send 3 messages an hour. After those 3, `allowed_now` is 0.125, which rounds down to 0 until `next_hour_at`:

```
$ curl -s localhost:8080/v1/limits/mx-5/gmail
{"source":"mx-5","provider":"gmail","managed":true,"state":"raising","reason":"new lane",
 "allowed_now":0.125,"limit":50,"sent":3,"remaining":47,"target":110000,"day":0,
 "hourly_limit":3.125,"sent_hour":3,"remaining_hour":0.125,"engaged_only":true,...}
```

## States

| State | Meaning |
|-------|---------|
| `raising` | Limit grew today, or the lane is new |
| `at_target` | Limit equals the target |
| `holding` | Limit unchanged. `reason` names the cause: complaints, bounces, low volume, a backoff, a block, or the end of a pause |
| `paused` | Complaints reached the pause line. Sends nothing for `pause_days` |
| `backoff` | Stopped for `backoff.duration` after rate limit replies |
| `blocked` | Stopped until the next day after reputation block replies |

## Configuration

All keys are optional. Unknown keys fail the start, and so do `growth`, `shaping` or `mix` entries that name no provider and negative targets.

`warmup:`

| Key | Default | Effect |
|-----|---------|--------|
| `start_limit` | 50 | First limit of a new lane |
| `growth` | default 1.4, gmail 1.5, outlook 1.3, yahoo 1.35 | Daily multiplier per provider |
| `grow_when_used` | 0.7 | Share of the limit a lane must send to grow. 0 grows on any clean day |
| `hold_complaint_ratio`, `pause_complaint_ratio` | 0.001, 0.003 | Complaints over deliveries that hold or pause |
| `hold_bounce_ratio` | 0.03 | Bounces over first attempts that hold |
| `hold_complaints`, `pause_complaints` | 2, 3 | Daily complaint counts that hold or pause below `min_sample` |
| `min_sample` | 50 | Deliveries in the window before the complaint ratios apply, and first attempts before the bounce ratio applies |
| `trailing_days` | 3 | Closed days the ratios cover, 1-14 |
| `pause_days` | 1 | Length of a pause |
| `resume_fraction` | 0.5 | Share of the last good limit after a pause or block |
| `idle_days_reset` | 30 | Days without a first attempt before the limit resets. 0 disables it |
| `hours_per_day` | 16 | Hourly limit below target is `limit / hours_per_day` |
| `decision_hour` | 0 | UTC hour a day starts |
| `day_length` | 24h | 1m to 24h. The compose stack uses 24m |
| `engaged_first` | true | Sets `engaged_only` while the lane is below target |
| `backoff.enabled` | true | false disables the backoff and block rules |
| `backoff.window`, `backoff.min_attempts`, `backoff.min_hits`, `backoff.ratio`, `backoff.duration` | 5m, 20, 5, 0.05, 10m | Rate limit rule |
| `backoff.block_ratio` | 0.01 | Reputation block rule, with the same window and minimums |
| `shaping` | default 10 x 100, gmail 5 x 50, outlook 5 x 50, yahoo 10 x 20, icloud 10 x 5 | `connections` x `per_connection` per provider |

`targets:`

| Key | Default | Effect |
|-----|---------|--------|
| `default` | 0 | Daily target of every source. 0 leaves sources unmanaged |
| `sources.<name>.daily` | | Target of one source |
| `sources.<name>.warm` | false | Source starts at its target |
| `warm` | | List of sources that start at their target |
| `mix` | | Share of the target per provider, summing to at most 1. Each provider not listed may use the whole remainder |

Top level:

| Key | Default | Effect |
|-----|---------|--------|
| `listen` | `:8080` | Listen address |
| `state_path` | `data/warmup.json` | Warmup snapshot file |
| `snapshot_interval` | 30s | Snapshot period. ehlo also saves on shutdown |
| `max_body_bytes` | 4 MiB | Request body limit. Larger bodies get 413 |
| `sources` | | Sources whose series start at zero and whose lanes the endpoint may create |
| `providers` | built-in list | Domain-to-provider rules. Replaces the built-in list. A replaced list must still name every provider that `growth`, `shaping` and `mix` name, including the defaults `gmail`, `outlook`, `yahoo` and `icloud` |
| `providers_extra` | | Rules added after `providers` or the built-in list. For a domain or suffix both list, this one wins. Each rule needs a `name` (1-32 letters, digits, `.`, `_` or `-`) and at least one domain; a leading dot matches the domain and its subdomains. Use it for Google Workspace or Microsoft 365 tenants, which the built-in list maps to `other` |
| `routes` | | Routes accepted besides `self_hosted`, `ses` and `sendgrid` |
| `max_sources` | 256 | Distinct sources accepted. Events from further sources are rejected |
| `webhooks.token` | | Every webhook request must carry `?token=<value>` |
| `webhooks.sendgrid_public_key` | | The SendGrid webhook must carry a valid signature. Base64 key from SendGrid's Signed Event Webhook settings |
| `webhooks.token_file` | | File whose content, trimmed of surrounding whitespace, is `webhooks.token`, such as a mounted Kubernetes Secret key. Read once at start. Setting it with `webhooks.token`, or naming an unreadable or empty file, fails the start |
| `webhooks.sendgrid_public_key_file` | | The same for `webhooks.sendgrid_public_key` |

## Commands

| Command | Flags |
|---------|-------|
| `ehlo serve` | `-config`, and `-listen`, `-state`, `-day-length` to override the file |
| `ehlo tail-postfix` | `-source` (required), `-target` (`http://127.0.0.1:8080`), `-file` (`-` for stdin), `-batch` (200), `-flush` (2s), `-tz` (`Local`), `-retry-after` (1m), `-skip-relay` |
| `ehlo probe <url>` | Exits 0 on HTTP 200. `-timeout` (3s) |
| `ehlo version` | |
