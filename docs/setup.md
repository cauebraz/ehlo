# Setup

## Demo

`just up` starts ehlo, a traffic simulator, Prometheus and Grafana (`http://localhost:3000`). The simulator sends 600,000 messages a day: 540,000 from four servers and 60,000 through SES and SendGrid. A warmup day lasts 24 minutes.

| Command | Effect |
|---------|--------|
| `just scenario gmail-throttle` | Gmail, Yahoo and Microsoft rate limit two servers |
| `just scenario connect-refused` | Outlook refuses connections |
| `just scenario complaint-spike` | Gmail complaints rise on one server |
| `just scenario warmup` | A new server joins and warms up |
| `just reset` | Deletes all volumes and returns to the steady scenario |

The justfile lists all eight scenarios. `just down` keeps the warmup state in the `ehlo-state` volume. If the logs show `permission denied` on the snapshot, the volume predates the current image; run `just reset` once.

## Install

From source, with Go 1.27 or later. Replace `<tag>` with a tag from the [releases page](https://github.com/cauebraz/ehlo/releases):

```
go install github.com/cauebraz/ehlo/cmd/ehlo@<tag>
curl -fsSLO https://raw.githubusercontent.com/cauebraz/ehlo/<tag>/ehlo.example.yaml
ehlo serve -config ehlo.example.yaml
```

The release archives below also carry `ehlo.example.yaml`. [examples/systemd/ehlo.service](../examples/systemd/ehlo.service) runs the server under systemd.

On Kubernetes, install the Helm chart: `helm install ehlo oci://ghcr.io/cauebraz/charts/ehlo --version <version>`. [kubernetes.md](kubernetes.md) covers the values.

Each release ships:

| File | Contents |
|------|----------|
| `ehlo_<version>_<os>_<arch>.tar.gz` | `ehlo` and `ehlo-sim` for Linux and macOS on amd64 and arm64, the example config, the Prometheus rules, the dashboards and these docs |
| `ehlo-monitoring_<version>_<os>_<arch>` | `ehlo-monitoring` for Linux, macOS (tar.gz) and Windows (zip) on amd64 and arm64 |
| `ehlo-monitoring-bundle_<version>.tar.gz` | `ehlo-monitoring render -format all` with the default config |
| `ehlo-dashboards-classic_<version>.tar.gz` | The dashboards as classic JSON for Grafana 12.4 and grafana.com; [kubernetes.md](kubernetes.md#grafana-12-and-grafanacom) lists what they lose |
| `ehlo-chart-<version>.tgz` | The Helm chart, also at `oci://ghcr.io/cauebraz/charts/ehlo` |
| `ghcr.io/cauebraz/ehlo:<version>` | Image for linux/amd64 and linux/arm64. It runs as a non-root user and keeps its state in `/data` |

Each release also has `checksums.txt` covering every file above, an SPDX SBOM per archive, and GitHub build provenance attestations for the files, the image and the chart. Verify them with the [GitHub CLI](https://cli.github.com):

```
sha256sum --ignore-missing -c checksums.txt
gh attestation verify --owner cauebraz ehlo_<version>_linux_amd64.tar.gz
gh attestation verify --owner cauebraz oci://ghcr.io/cauebraz/ehlo:<version>
gh attestation verify --owner cauebraz oci://ghcr.io/cauebraz/charts/ehlo:<version>
```

The image also carries BuildKit provenance and SBOM attestations: `docker buildx imagetools inspect ghcr.io/cauebraz/ehlo:<version> --format '{{ json .Provenance }}'`.

## Configure

ehlo needs to know each sending server, called a source, and the daily volume it should reach. [limits.md](limits.md#terms) defines the terms.

1. Set `targets.default` to the daily volume each server should reach. At the default 0, ehlo manages no lane.
2. List your servers under `sources`.
3. List servers that already have a reputation under `targets.warm`. Other servers start at 50 messages a day.

[ehlo.example.yaml](../ehlo.example.yaml) shows each key. [limits.md](limits.md#configuration) describes them.

## Send events

| From | How |
|------|-----|
| Postfix | `tail -F /var/log/mail.log \| ehlo tail-postfix -source $(hostname)` |
| Postfix without rsyslog (Debian 12+) | `examples/systemd/ehlo-tail-postfix.service`, which reads `journalctl -o short-iso` |
| KumoMTA | Log hook posting to `/v1/webhooks/kumomta`, see [integrations.md](integrations.md#kumomta) |
| PowerMTA or any MTA log | Vector configs in `examples/vector` |
| Your code | `POST /v1/events`, see [events.md](events.md) |
| Amazon SES | SNS HTTPS subscription to `/v1/webhooks/ses`. ehlo logs the `subscribe_url` instead of calling it; open it or paste it in the SNS console |
| SendGrid | Event Webhook to `/v1/webhooks/sendgrid`, with Signed Event Webhook on and `webhooks.sendgrid_public_key` set |

## Ask before sending

ehlo does not hold mail back. To pace a warmup, the code that picks a source for each batch asks `GET /v1/limits/{source}/{provider}` for each candidate source, gives each source at most `allowed_now` messages, and queues the rest or sends it through an ESP. [limits.md](limits.md#from-a-sender) shows the loop and every field. Skip this step to use ehlo for monitoring only.

## Monitor

Scrape `/metrics`, load `prometheus/rules/ehlo-recording.yml` and `ehlo-alerts.yml`, and provision `grafana/dashboards/` with a file provider. These files are the default render. For another job name, a cluster label, a datasource, Grafana-managed alerts or Kubernetes objects, render your own with `ehlo-monitoring render`; [monitoring.md](monitoring.md) shows how. Every alert carries the label `component="ehlo"`, and the dashboards count only alerts with it, so a shared Prometheus works.

Run one ehlo replica per warmup state: each process keeps its own lanes, so two replicas split the events and their limits diverge. [monitoring.md](monitoring.md#one-replica) explains why.

The dashboards use the Grafana dashboard schema v2 (`apiVersion: dashboard.grafana.app/v2`) and need Grafana 13 or later; the demo pins 13.3.1. Panels sit in auto grids that reflow on narrow screens, and each panel has a one-line subtitle under its title. The `monitoring` package generates the files; do not edit the JSON or the rules by hand.

There are nine dashboards: Overview, Delivery, Reputation, Warmup, one each for Gmail, Microsoft and Yahoo, Reply codes, and Pipeline. [dashboards.md](dashboards.md) describes each one with a screenshot. [alerts.md](alerts.md) explains each alert.

## Security

`/v1/events` and `/v1/limits` have no authentication. Keep them on a private network or behind a proxy that checks callers.

Expose only `/v1/webhooks/` to the internet. Set `webhooks.token` (or `webhooks.token_file`), add `?token=<value>` to every webhook URL (SES, SendGrid, KumoMTA), and set `webhooks.sendgrid_public_key` (or `webhooks.sendgrid_public_key_file`). See [SECURITY.md](../SECURITY.md).
