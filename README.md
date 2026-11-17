# ehlo

[![ci](https://github.com/cauebraz/ehlo/actions/workflows/ci.yml/badge.svg)](https://github.com/cauebraz/ehlo/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cauebraz/ehlo/badge)](https://scorecard.dev/viewer/?uri=github.com/cauebraz/ehlo)
[![Go Reference](https://pkg.go.dev/badge/github.com/cauebraz/ehlo.svg)](https://pkg.go.dev/github.com/cauebraz/ehlo)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

ehlo tells your mail servers how much they can send to each mailbox provider, and paces the warmup of new IPs.

It reads delivery events from Postfix, Amazon SES or SendGrid, sorts SMTP replies into categories, exports Prometheus metrics, and answers before each send:

```
$ curl -s localhost:8080/v1/limits/mx-1/gmail
{"source":"mx-1","provider":"gmail","state":"at_target","allowed_now":103978,"limit":110000,"sent":6022,...}
```

## Who it is for

Teams that run their own mail servers (Postfix or another MTA) or send through SES or SendGrid, and need to:

- warm up new sending IPs without tripping Gmail, Microsoft or Yahoo limits,
- see when a provider rate limits, blocks or defers their mail, and why,
- alert on complaint and bounce ratios before a provider does.

## Try it in five minutes

Requires Docker with Compose v2, [just](https://github.com/casey/just) and `openssl` (used once to create a Grafana password).

```
git clone https://github.com/cauebraz/ehlo.git && cd ehlo
just up                        # ehlo, simulator, Prometheus and Grafana; waits until healthy
open http://localhost:3000     # Grafana, read-only without login; opens on the Overview dashboard
just scenario gmail-throttle   # Gmail, Yahoo and Microsoft rate limit two servers
just reset                     # delete the data and go back to the steady scenario
```

The demo listens on `127.0.0.1` only: ehlo on 8080, Prometheus on 9090, Grafana on 3000. A warmup day lasts 24 minutes. [Setup](docs/setup.md#demo) lists the scenarios.

## Kubernetes

```
helm install ehlo oci://ghcr.io/cauebraz/charts/ehlo --version <version> \
  --namespace ehlo --create-namespace \
  --set serviceMonitor.enabled=true --set serviceMonitor.labels.release=<kube-prometheus-stack release> \
  --set prometheusRule.enabled=true --set prometheusRule.labels.release=<kube-prometheus-stack release> \
  --set dashboards.enabled=true
```

The chart runs one replica with its warmup state on a volume. [Kubernetes](docs/kubernetes.md) covers values, webhook secrets, the Grafana Operator and Mimir.

## Already have Prometheus and Grafana

Point Prometheus at ehlo's `/metrics`, then render the rules and dashboards for your stack with `ehlo-monitoring`, shipped in each release:

```
ehlo-monitoring render -selector 'job="mail-ehlo"' -format all -out ehlo-monitoring/
```

It writes Prometheus rule files, a Mimir rule file, schema v2 dashboards, and PrometheusRule, ConfigMap and GrafanaManifest objects. [Monitoring](docs/monitoring.md) lists the options. For Grafana 12, import the classic dashboards attached to each release.

## Requirements

| Component | Version |
|-----------|---------|
| Go, to build from source | 1.27 or later |
| Prometheus, for the rules and alerts | 3.x; CI tests them with 3.16 |
| Grafana, to import the dashboards | 13 or later. The dashboards use the Grafana dashboard schema v2, which older versions cannot import. Releases also attach classic copies, tested on 12.4 |
| Kubernetes, for the Helm chart | 1.30 or later; CI tests the chart with Helm 4.3.2 |
| Grafana Operator, for `GrafanaManifest` objects | v5.25 or later |

Releases ship Linux and macOS binaries for amd64 and arm64, `ehlo-monitoring` for Linux, macOS and Windows, the image `ghcr.io/cauebraz/ehlo` for linux/amd64 and linux/arm64, and the Helm chart `oci://ghcr.io/cauebraz/charts/ehlo`. Each release has SBOMs, a checksum file and GitHub build provenance attestations; [Setup](docs/setup.md#install) shows how to verify them.

## Docs

- [Setup](docs/setup.md): install, configure, send events, monitor
- [Integrations](docs/integrations.md): feed ehlo from Postfix, KumoMTA, PowerMTA, SES, SendGrid or any MTA
- [Kubernetes](docs/kubernetes.md): Helm chart, kube-prometheus-stack, Grafana Operator, Mimir
- [Monitoring](docs/monitoring.md): render rules and dashboards for your stack
- [Limits API](docs/limits.md): how warmup works and every config key
- [Events](docs/events.md): event format
- [Metrics](docs/metrics.md): metrics and reply categories
- [Alerts](docs/alerts.md): what each alert means and what to do
- [Changelog](CHANGELOG.md)
- [Contributing](CONTRIBUTING.md), [Code of Conduct](CODE_OF_CONDUCT.md), [Security](SECURITY.md)

## Status

Pre-1.0: event fields, metric names and config keys may change between minor releases. [CHANGELOG.md](CHANGELOG.md) lists each change.

## License

[MIT](LICENSE)
