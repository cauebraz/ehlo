# Integrations

Each path below ends in the same event API. [events.md](events.md) has the event fields and the exact mapping of each adapter.

| Path | Fills | Cannot fill |
|------|-------|-------------|
| [Postfix with rsyslog](#postfix-with-rsyslog) | attempts (first, retry), replies with code, enhanced status, stage and delay, bounces, TLS sessions | `signed`, `engaged`, queue, complaints, DMARC |
| [Postfix with journald](#postfix-with-journald) | Same as rsyslog | Same as rsyslog |
| [KumoMTA](#kumomta) | attempts, replies, bounces; expirations with `source=egress_pool`; complaints from ARF reports with the [trace setup](#attribute-kumomta-complaints) | `signed`, `engaged`, queue, TLS sessions, DMARC, out-of-band bounces |
| [PowerMTA through Vector](#powermta-through-vector) | attempts, replies, bounces, complaints from `f` records | `signed`, `engaged`, queue, TLS sessions, DMARC; complaints land on source `powermta` |
| [Any MTA through the HTTP API](#any-mta-through-the-http-api) | Whatever the MTA logs | - |
| [Amazon SES](#amazon-ses) | sends, deliveries, bounces, complaints, opens on route `ses` | `signed`, `engaged`, deferrals, queue, DMARC |
| [SendGrid](#sendgrid) | processed, deliveries, deferrals with reply text, bounces, spam reports, opens on route `sendgrid` | `signed`, `engaged`, queue, DMARC |

[Fill the gaps](#fill-the-gaps) covers what no adapter sends.

## Postfix with rsyslog

1. Install the binary in `/usr/local/bin/ehlo` and run the server: [examples/systemd/ehlo.service](../examples/systemd/ehlo.service).
2. Copy [examples/systemd/ehlo-tail-postfix-file.service](../examples/systemd/ehlo-tail-postfix-file.service) to `/etc/systemd/system/`.
3. Write `EHLO_SOURCE=<this host>` and `EHLO_TARGET=http://<ehlo>:8080` to `/etc/default/ehlo-tail-postfix`.
4. `systemctl enable --now ehlo-tail-postfix-file.service`.

The unit tails `/var/log/mail.log` from its end as a dynamic user in group `adm`. A smarthost needs `-skip-relay`; add it to `ExecStart`.

## Postfix with journald

Debian 12 and later, and Ubuntu installs without rsyslog, have no `/var/log/mail.log`; Postfix logs only to the journal.

1. Install the binary and run the server as above.
2. Copy [examples/systemd/ehlo-tail-postfix.service](../examples/systemd/ehlo-tail-postfix.service) to `/etc/systemd/system/`.
3. Write `/etc/default/ehlo-tail-postfix` as above.
4. `systemctl enable --now ehlo-tail-postfix.service`.

The unit runs `journalctl --follow --lines=0 --output=short-iso --unit="postfix*"` into `ehlo tail-postfix`. Lines logged while the unit is stopped are not read. [examples/systemd/README.md](../examples/systemd/README.md) lists the hardening and how it was tested.

## KumoMTA

1. Set `webhooks.token` (or `webhooks.token_file`) in the ehlo config.
2. Add a log hook to KumoMTA's `init.lua` that posts to `https://<ehlo>/v1/webhooks/kumomta?token=<token>`: [examples/kumomta/README.md](../examples/kumomta/README.md) has a one-record and a batched hook.
3. Give the hook's queue its own shaping entry.
4. With more than 256 egress sources, add `&source=egress_pool` or raise `max_sources`.

Events keep route `self_hosted`, so KumoMTA lanes get warmup limits like Postfix ones. A record without the source field (`egress_source`, or `egress_pool` with `&source=egress_pool`) gives no events, so ehlo never creates a lane the attempts do not use. KumoMTA logs `OOB` records with neither field, so out-of-band bounces are not counted. `Expiration` records name only `egress_pool` and count with `&source=egress_pool`. KumoMTA does not log whether the peer certificate was verified, so ehlo counts no TLS sessions from it.

### Attribute KumoMTA complaints

`Feedback` records carry no egress fields either. ehlo takes the source from `feedback_report.supplemental_trace`, which KumoMTA decodes from the `X-KumoRef` header of the reported message. That header holds the message meta listed in `trace_headers.include_meta_names`. To count complaints on the lane of their attempts:

1. Post to `/v1/webhooks/kumomta?source=egress_pool`. KumoMTA picks the egress source at delivery, after the header is written, so only the pool is known in time.
2. In `smtp_server_message_received`, after the queues helper sets `tenant`, set meta `egress_pool` to the pool that the queues helper config (`queue.toml`) assigns to the message's tenant.
3. List that meta in the listener's trace headers:

```lua
local pool_by_tenant = { ['tenant-a'] = 'pool0' }

kumo.on('smtp_server_message_received', function(msg)
  queue_helper:apply(msg)
  local pool = pool_by_tenant[msg:get_meta 'tenant']
  if pool then
    msg:set_meta('egress_pool', pool)
  end
end)

kumo.start_esmtp_listener {
  listen = '0.0.0.0:25',
  trace_headers = { include_meta_names = { 'egress_pool' } },
}
```

For messages injected over HTTP, set the meta in `http_message_generated` and pass the same `trace_headers` in the injection request. When each pool has one source, setting meta `egress_source` works with the default `source=egress_source`. The header is base64, not encrypted: recipients can read the pool name. A complaint without the traced field gives no event.

## PowerMTA through Vector

1. Add the two `<acct-file>` blocks from [examples/vector/README.md](../examples/vector/README.md#powermta) to the PowerMTA config.
2. Install Vector 0.62.0 and copy [examples/vector/powermta/vector.yaml](../examples/vector/powermta/vector.yaml); set `uri` to your ehlo.
3. Start Vector. It reads the accounting files from their end and posts NDJSON to `/v1/events`.

The field mapping follows the PowerMTA 5.0 User's Guide and is not verified against a running PowerMTA.

## Any MTA through the HTTP API

1. Pick a `source` per sending IP or pool, 1-64 letters, digits, `.`, `_`, `:` or `-`.
2. For each delivery attempt, post an `attempt` and the provider's `reply` to `POST /v1/events`, one JSON object per line, up to 10,000 per request.
3. Post a `bounce` when the MTA gives up on a recipient and a `complaint` per feedback report.
4. A log shipper does the same without code: [examples/vector/json/vector.yaml](../examples/vector/json/vector.yaml) maps a JSON log line.

## Amazon SES

1. Create an SNS topic and set it as the destination of a configuration set's event publishing (or of identity notifications).
2. Subscribe `https://<ehlo>/v1/webhooks/ses?token=<token>` over HTTPS.
3. ehlo logs the `subscribe_url`; open it or paste it in the SNS console to confirm.

## SendGrid

1. In Mail Settings, Event Webhook, set the URL to `https://<ehlo>/v1/webhooks/sendgrid?token=<token>`.
2. Turn on Signed Event Webhook and set `webhooks.sendgrid_public_key` (or `webhooks.sendgrid_public_key_file`) to the key it shows.
3. Select the processed, delivered, deferred, bounce, spam report and open events.

## Fill the gaps

| Field | How to send it |
|-------|----------------|
| `signed` | Set it on the `attempt` from the code that signs, or from the MTA's DKIM log line |
| `engaged` | Set it on the `attempt` from your engagement data |
| Queue | A cron job that posts `{"type":"queue","source":...,"deferred":N,"oldest_age":ns}` per source; for Postfix, count `postqueue -j` entries with a `delay_reason` |
| Complaints for Postfix | Post a `complaint` per ARF report from the mailbox that receives your feedback loops. Without them the complaint ratio reads 0 |
| DMARC | Post one `dmarc` row per record of each aggregate report |
| Providers for Workspace and Microsoft 365 tenants | List their domains under `providers_extra` ([limits.md](limits.md)) |
