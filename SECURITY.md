# Security

## Supported versions

| Version | Supported |
|---------|-----------|
| Latest minor release | Yes |
| Older releases | No |

Before 1.0, fixes ship in a new release of the latest minor version only.

## Reporting a vulnerability

Report it privately through GitHub: open the [Security tab](https://github.com/cauebraz/ehlo/security) and choose "Report a vulnerability", or go to https://github.com/cauebraz/ehlo/security/advisories/new. Do not open a public issue, discussion or pull request.

Include the version, the affected endpoint or file, and the steps or request that reproduce it.

You get a first reply within five days. The fix, the advisory and the CVE request, when one applies, are coordinated with you in the advisory.

## Verifying releases

Release archives and the GHCR image carry GitHub build provenance attestations, and each archive has an SPDX SBOM. [docs/setup.md](docs/setup.md#release-files) shows the `gh attestation verify` commands.

## Deployment

- `/v1/events` and `/v1/limits` have no authentication. Run ehlo on a private network or behind a proxy that checks callers. The compose file listens on localhost only.
- The SES, SendGrid and KumoMTA webhooks must be reachable from their senders. Expose only `/v1/webhooks/`, set `webhooks.token`, send it in the `Authorization` header (Bearer, or Basic with the token as password) where the sender allows, since a `?token=` query string appears in proxy access logs, and set `webhooks.sendgrid_public_key` to check SendGrid's signature. Both can be read from files (`webhooks.token_file`, `webhooks.sendgrid_public_key_file`) so they stay out of the config. ehlo does not verify SNS signatures, because that needs an outbound call to fetch the certificate; the token is the check for SES.
- ehlo makes no outbound connections.
- Request bodies are size-limited, and a request to any endpoint, webhooks included, gives at most 10,000 events; a request over either limit gets 413 and records nothing. A KumoMTA record counts up to three events per recipient domain toward that limit. Every event is validated.
- Label values a client names are bounded: an unknown provider becomes `other`, routes must be configured, sources are shape-checked and capped by `max_sources` (256), and the reply code, enhanced status and tag are shape-checked. `ehlo_smtp_replies_total` caps its series per lane and in total, see [docs/metrics.md](docs/metrics.md#when-series-appear).
- Recipient addresses are reduced to their domain and never become labels.

## Known limitations

- ehlo checks the SendGrid signature but not the age of `X-Twilio-Email-Event-Webhook-Timestamp`, so a captured signed request can be replayed and counted again. SendGrid's [security features page](https://www.twilio.com/docs/sendgrid/for-developers/tracking-events/getting-started-event-webhook-security-features) gives no maximum age, and its [Event Webhook page](https://www.twilio.com/docs/sendgrid/for-developers/tracking-events/getting-started-event-webhook) retries failed posts for up to 24 hours without saying whether a retry is signed with a new timestamp. An age check could therefore reject valid retries. Keep the webhook token secret and serve the webhook over TLS only.
