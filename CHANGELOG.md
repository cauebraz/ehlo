# Changelog

All notable changes to ehlo are listed here. The format follows [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org). Before 1.0, minor releases can change event fields, metric names and config keys.

## [Unreleased]

First version.

### Added

- Event API, Postfix log tailer, and SES and SendGrid webhooks.
- SMTP replies sorted into categories such as rate limited, reputation block and authentication failure.
- Warmup limits per source and provider: daily growth gated on volume sent, trailing-window complaint and bounce ratios, holds, pauses, intraday backoffs on rate limiting and day-long stops on reputation blocks, hourly limits while warming.
- Prometheus metrics, alerts and nine Grafana dashboards.
- Dashboards in the Grafana dashboard schema v2. Importing them requires Grafana 13 or later.
- Dashboards written for readers outside infrastructure: a status in words, plain titles with a one-line subtitle, tiles that reflow on narrow screens, complaints per 10,000 and volumes per minute, neutral colors for plain counts, and technical detail in collapsed rows. They read only alerts labeled `component="ehlo"`.
- Traffic simulator with eight scenarios.
- `ehlo tail-postfix -skip-relay` for smarthosts, retries counted per recipient.
- A policy key set to 0 in the config stays 0: `idle_days_reset: 0` and `grow_when_used: 0` disable their rules.
- Ingest bounds client-named labels: an unknown `provider` becomes `other`, a `route` must be built in or listed under `routes`, and `source` is shape-checked and capped by `max_sources` (default 256).
- Webhook authentication: `webhooks.token` for every webhook and `webhooks.sendgrid_public_key` for SendGrid's signature. ehlo logs the SNS `subscribe_url` for an operator to confirm.
- The hourly limit applies per policy hour (`day_length/24`), counted from the day start. A lane resumes after a pause or block at no more than its target.
- SES events are dated by their own timestamp, suppressed addresses are skipped, and an open is one event. SendGrid `blocked` bounces are soft, `dropped` is skipped, and `deferred` keeps the reply text.
- The Postfix tailer keeps the provider code on greeting refusals and EHLO replies, reads RFC 5424 lines, skips `retry(8)` and lines over 1 MiB, and drops batches the server refuses instead of resending them.
- Microsoft `5.7.507` is a reputation block and `5.7.510` a policy reply, not an invalid recipient.
- Replies that name a missing recipient after the word, such as "mailbox not found" and Yahoo's "This user doesn't have a yahoo.com account", are `invalid_recipient` instead of `other`.
- The container image creates `/data` for the non-root user, so the warmup snapshot persists.
- Request bounds: `POST /v1/events` takes at most 10,000 events (413 above, and nothing is recorded), an empty batch gets 400, request headers are capped at 64 KiB (431), error messages at 256 bytes, and cross-origin browser POSTs get 403. The SendGrid webhook takes at most 10,000 events.
- Series bounds: `ehlo_events_total` counts unknown event types as `type="unknown"`. `ehlo_smtp_replies_total` keeps at most 64 detailed codes per route, source and provider and 4,096 other code series; past those bounds a reply counts under its bare code or its class (`4xx`), and documented provider codes are always kept. Reply codes 300-399 are invalid.
- Queue gauges disappear 15 minutes after a source's last `queue` event. Provider tags longer than 24 characters are dropped instead of rejecting the event.
- Alerts carry `component="ehlo"`, a plain-language summary, a description that starts with what to do, an absolute `runbook_url`, a `dashboard_uid` and an optional `dashboard_url`. Rate limit and block alerts use `keep_firing_for`, ratio alerts need at least 5 matching events, and `EhloQueueFeedStale` fires when a source stops reporting its queue. `ehlo:lane_quota_used:ratio` has no sample for a lane with limit 0.
- Alerts and the ratios behind them count the first events of a source absent from `sources`, whose series start with that event: one burst of block replies or the first complaints fire them. A new Prometheus does not count ehlo's lifetime totals as new.
- Config validation rejects NaN and infinite numbers, an empty `state_path`, negative per-source targets, and `growth`, `shaping` or `mix` keys that name no provider.
- Release archives for Linux and macOS on amd64 and arm64, with SPDX SBOMs, `checksums.txt` and GitHub build provenance attestations. A multi-arch GHCR image with OCI labels, BuildKit provenance and SBOM attestations, and a GitHub build provenance attestation.
- CI runs CodeQL, OpenSSF Scorecard, dependency review, govulncheck, actionlint and `goreleaser check`.
- Built with Go 1.27.4 on `distroless/static-debian13`. The demo stack runs Prometheus 3.16.0 and Grafana 13.3.1, and every image and action is pinned by digest or commit SHA.
- Helm chart `oci://ghcr.io/cauebraz/charts/ehlo`: one replica with the `Recreate` strategy and the warmup state on a PVC, a config from values or from an existing Secret, webhook credentials from a mounted Secret (`webhookSecret`), a non-root read-only pod, and optional ServiceMonitor or PodMonitor, PrometheusRule, Grafana sidecar dashboard ConfigMaps and Grafana Operator GrafanaManifest objects. Docs in `docs/kubernetes.md`.
- CI lints the chart and validates it with kubeconform against Kubernetes 1.30 and 1.37 schemas and the Prometheus Operator and Grafana Operator CRDs, and installs it with kube-prometheus-stack on kind (`just chart`, `just chart-e2e`).
- Releases push the chart to GHCR with a build provenance attestation, pin the image by digest in it, and attach the chart, the `ehlo-monitoring` binaries, the default monitoring render and classic dashboards for Grafana 12.4, all listed in `checksums.txt`.
- `ehlo-monitoring` and the public Go package `github.com/cauebraz/ehlo/monitoring` (v0) render the dashboards and rules for another stack: `selector`, `group_by`, `datasource`, `uid_prefix`, `alerts_metric` (`ALERTS` or `GRAFANA_ALERTS`), `rule_labels`, `runbook_url`, `dashboard_url` and Kubernetes options, in Prometheus, Mimir, PrometheusRule, ConfigMap and GrafanaManifest formats. Equality matchers in `selector` are kept on recorded series and alerts, so two renders can share one Prometheus. `render -clean` removes dashboards from an earlier render. Options are validated, including Kubernetes names and labels. `check` and `update` keep the committed default render in sync.
- Rules and dashboards work with any job name and with more than one ehlo target: `EhloTelemetryAbsent` keys on `ehlo_build_info`, gauges are read once per lane with `max by`, and recording rules aggregate away `instance`. The datasource variable defaults to Grafana's default data source.
- `scripts/export-classic.sh` (`just classic`) converts the schema v2 dashboards to classic JSON with `__inputs` and `__requires`, and checks that they import into Grafana 12.4.15 and 13.3.1.
- `POST /v1/webhooks/kumomta` takes KumoMTA log records (one object, an array or NDJSON) on route `self_hosted`, with `?source=egress_pool` to group by pool. A request yields at most 10,000 events (413 above). Records without the chosen source field give no events: `OOB` never counts, `Expiration` counts with `?source=egress_pool`, and complaints count when the egress source is in the `X-KumoRef` trace meta.
- `providers_extra` adds domain rules after the built-in list instead of replacing it. Provider rules are validated at start.
- `webhooks.token_file` and `webhooks.sendgrid_public_key_file` read the webhook secrets from files, such as mounted Kubernetes Secret keys.
- The PowerMTA Vector example tells a retry from a first attempt by `timeFirstAttempt`, so first attempts that waited in a throttled queue count against the warmup limit. Without the field every attempt counts.
- `docs/integrations.md` and `examples/`: systemd units for `ehlo serve` and the Postfix tailer (journald and `/var/log/mail.log`), KumoMTA log hooks, and Vector configs for PowerMTA accounting files and JSON MTA logs.
- `ehlo tail-postfix` is tested on `journalctl -o short-iso` lines.

[Unreleased]: https://github.com/cauebraz/ehlo/commits/main/
