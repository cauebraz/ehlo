# Setup

The simplest setup is one small Linux host, or your mail server itself, running ehlo next to Prometheus and Grafana. ehlo is one static binary with no database: its state is a JSON file. For Kubernetes, see [kubernetes.md](kubernetes.md).

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

## Requirements

| Component | Version |
|-----------|---------|
| Linux or macOS | amd64 or arm64; the binary is static and needs no other package |
| Prometheus, for the rules and alerts | 3.x; CI tests them with 3.16 |
| Grafana, to import the dashboards | 13 or later. The dashboards use the Grafana dashboard schema v2, which older versions cannot import. Releases also attach classic copies, tested on 12.4 |
| Go, only to build from source | 1.27 or later |

## 1. Install

Download the archive for your platform from the [releases page](https://github.com/cauebraz/ehlo/releases). It holds the `ehlo` binary, `ehlo.example.yaml`, the Prometheus rules, the dashboards and these docs.

```
curl -fsSLO https://github.com/cauebraz/ehlo/releases/download/v<version>/ehlo_<version>_linux_amd64.tar.gz
mkdir ehlo && tar -xzf ehlo_<version>_linux_amd64.tar.gz -C ehlo
sudo install ehlo/ehlo /usr/local/bin/ehlo
```

From source instead: `go install github.com/cauebraz/ehlo/cmd/ehlo@v<version>`.

## 2. Configure

ehlo needs your sending servers, called sources, and the daily volume each should reach. Write `/etc/ehlo/ehlo.yaml`:

```yaml
sources: [mx-1, mx-2]
targets:
  default: 200000
  warm: [mx-1]
```

`targets.default` is the daily volume each server should reach, and `targets.warm` lists servers that already have a reputation. `mx-1` starts at its target. `mx-2` is new: it starts at 50 messages a day and grows after each clean day. [ehlo.example.yaml](../ehlo.example.yaml) shows every key and [limits.md](limits.md#configuration) describes them.

## 3. Run

```
ehlo serve -config /etc/ehlo/ehlo.yaml -state /var/lib/ehlo/warmup.json
```

ehlo listens on `:8080` and saves its state every 30 seconds and on shutdown. [examples/systemd/ehlo.service](../examples/systemd/ehlo.service) runs it under systemd with that command line. Run one ehlo process for all your servers: two processes would each count only part of the sends ([why](monitoring.md#one-replica)).

## 4. Send events

On each Postfix server, feed the mail log to ehlo:

```
tail -F /var/log/mail.log | ehlo tail-postfix -source mx-1 -target http://<ehlo host>:8080
```

[integrations.md](integrations.md) has the systemd units for this, and the setup for the other paths:

| From | How |
|------|-----|
| Postfix without rsyslog (Debian 12+) | `examples/systemd/ehlo-tail-postfix.service`, which reads `journalctl -o short-iso` |
| KumoMTA | Log hook posting to `/v1/webhooks/kumomta` |
| PowerMTA or any MTA log | Vector configs in `examples/vector` |
| Your code | `POST /v1/events`, see [events.md](events.md) |
| Amazon SES | SNS HTTPS subscription to `/v1/webhooks/ses`. ehlo logs the `subscribe_url` instead of calling it; open it or paste it in the SNS console |
| SendGrid | Event Webhook to `/v1/webhooks/sendgrid`, with Signed Event Webhook on and `webhooks.sendgrid_public_key` set |

## 5. Ask before sending

Before each batch, the code that picks a server asks ehlo how much it may send:

```
$ curl -s localhost:8080/v1/limits/mx-2/gmail
{"source":"mx-2","provider":"gmail","state":"raising","reason":"new lane","allowed_now":3.125,"limit":50,"sent":0,...}
```

Send at most `allowed_now` messages through `mx-2` to Gmail, and route the rest through another server or queue it. ehlo does not hold mail back itself. [limits.md](limits.md#from-a-sender) shows the loop and every field. Skip this step to use ehlo for monitoring only.

## 6. Monitor

Add ehlo to `prometheus.yml` and load the two rule files from the archive:

```yaml
rule_files:
  - rules/ehlo-recording.yml
  - rules/ehlo-alerts.yml

scrape_configs:
  - job_name: ehlo
    static_configs:
      - targets: ["<ehlo host>:8080"]
```

In Grafana, provision the JSON files from `grafana/dashboards/` with a file provider, as [grafana/provisioning/dashboards/ehlo.yaml](../grafana/provisioning/dashboards/ehlo.yaml) does in the demo. The dashboards need Grafana 13 or later.

There are nine dashboards: Overview, Delivery, Reputation, Warmup, one each for Gmail, Microsoft and Yahoo, Reply codes, and Pipeline. [dashboards.md](dashboards.md) describes each one with a screenshot. [alerts.md](alerts.md) explains each alert. Every alert carries the label `component="ehlo"`, and the dashboards count only alerts with it, so a shared Prometheus works.

These files are the default render. For another job name, a cluster label, a datasource or Grafana-managed alerts, render your own with `ehlo-monitoring render`; [monitoring.md](monitoring.md) shows how. The `monitoring` package generates the files; do not edit the JSON or the rules by hand.

## Security

`/v1/events` and `/v1/limits` have no authentication. Keep them on a private network or behind a proxy that checks callers.

Expose only `/v1/webhooks/` to the internet. Set `webhooks.token` (or `webhooks.token_file`), send it with every webhook request (SES, SendGrid, KumoMTA) as `?token=<value>`, as `Authorization: Bearer <value>`, or as the password of HTTP Basic authentication; the header keeps it out of proxy access logs, and set `webhooks.sendgrid_public_key` (or `webhooks.sendgrid_public_key_file`). See [SECURITY.md](../SECURITY.md).

## Release files

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
