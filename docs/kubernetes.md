# Kubernetes

The Helm chart in [deploy/helm/ehlo](../deploy/helm/ehlo) runs the ehlo server with its state on a volume, and can add a ServiceMonitor or PodMonitor, a PrometheusRule, dashboard ConfigMaps for the Grafana sidecar, and GrafanaManifest objects for the Grafana Operator. CI tests it with Helm 4.3.2 on Kubernetes 1.30 schemas and a 1.37 kind cluster. The Postfix tailer (`ehlo tail-postfix`) runs on the mail hosts, not in the cluster.

## Install

Each release pushes the chart to `oci://ghcr.io/cauebraz/charts/ehlo` with the same version as the image, and attaches it to the GitHub release as `ehlo-chart-<version>.tgz`. A released chart references the image by tag and digest.

```
helm install ehlo oci://ghcr.io/cauebraz/charts/ehlo --version <version> \
  --namespace ehlo --create-namespace -f values.yaml
```

Pin the chart by digest with `oci://ghcr.io/cauebraz/charts/ehlo:<version>@sha256:<digest>`; Helm refuses a chart whose digest differs. Verify the provenance attestation with `gh attestation verify --owner cauebraz oci://ghcr.io/cauebraz/charts/ehlo:<version>`.

A minimal `values.yaml`:

```yaml
config:
  sources: [mx-1, mx-2]
  targets:
    default: 200000
    warm: [mx-1]
```

`config` is `ehlo.yaml` ([limits.md](limits.md#configuration) lists every key). The chart sets `listen: ":8080"` and runs `ehlo serve -state /data/warmup.json`.

## One replica

The Deployment runs one replica with the `Recreate` strategy, and the chart has no `replicas` value. ehlo keeps each lane's limit, sent count, holds and backoffs in memory and writes them to `/data/warmup.json` every 30 seconds and on shutdown. Two replicas would count sends separately, grow limits on half the data, and overwrite each other's snapshot. `Recreate` stops the old pod before the new one mounts the `ReadWriteOnce` volume.

A restart loses at most the last 30 seconds of counts. With `persistence.enabled: false` the state lives in an `emptyDir` and every lane starts again at `warmup.start_limit` after a reschedule. The PVC carries `helm.sh/resource-policy: keep`, so `helm uninstall` leaves it in place.

## Webhook secrets

The chart refuses to render `webhooks.token` or `webhooks.sendgrid_public_key` into a ConfigMap. Keep them in a Secret and let the chart mount it; ehlo reads them through `webhooks.token_file` and `webhooks.sendgrid_public_key_file`, as UID 65532 from the mounted files:

```
kubectl -n ehlo create secret generic ehlo-webhooks --from-literal=token=<token> --from-file=sendgrid.pub=./sendgrid.pub
```

```yaml
webhookSecret:
  name: ehlo-webhooks
  tokenKey: token
  sendgridPublicKeyKey: sendgrid.pub
```

ehlo reads the files once at start. Kubernetes updates the mounted files after a change to the Secret, but ehlo keeps the old values until `kubectl -n ehlo rollout restart deployment/ehlo`. The chart cannot add a checksum annotation for a Secret it does not render, so run the restart after every rotation.

To keep the whole config in a Secret instead, store `ehlo.yaml` there:

```
kubectl -n ehlo create secret generic ehlo-config --from-file=ehlo.yaml=./ehlo.yaml
```

```yaml
existingConfigSecret: ehlo-config
existingConfigSecretKey: ehlo.yaml
```

With `existingConfigSecret` the chart ignores `config`, and rendering fails when `webhookSecret.name` is also set: the chart cannot add `webhooks.token_file` to a file it does not render, so put the webhook credentials in the config Secret. The file must omit `listen` or set it to `:8080`; the chart passes `-state /data/warmup.json`, which overrides `state_path`. A change to the Secret takes effect after `kubectl -n ehlo rollout restart deployment/ehlo`. With External Secrets Operator, a template can build the file from the token in your secret store. Expose only `/v1/webhooks/` outside the cluster; [SECURITY.md](../SECURITY.md) explains why.

## kube-prometheus-stack

kube-prometheus-stack selects ServiceMonitors, PodMonitors and PrometheusRules labeled `release: <its release name>`, and its Grafana sidecar loads ConfigMaps labeled `grafana_dashboard: "1"` from every namespace. With a stack installed as `kps`:

```yaml
serviceMonitor:
  enabled: true
  labels:
    release: kps
prometheusRule:
  enabled: true
  labels:
    release: kps
dashboards:
  enabled: true
  annotations:
    grafana_folder: ehlo   # with grafana.sidecar.dashboards.folderAnnotation: grafana_folder
```

The dashboards use the Grafana dashboard schema v2 and need Grafana 13 or later. kube-prometheus-stack 93.3.1 ships Grafana 13.3.1. The bundled rules and dashboards do not filter on `job`; `serviceMonitor.job` sets that label when your own queries expect one.

`deploy/scripts/kind-e2e.sh` (`just chart-e2e`) installs kube-prometheus-stack 93.3.1 with [deploy/kind/kube-prometheus-stack.yaml](../deploy/kind/kube-prometheus-stack.yaml) and this chart on a kind cluster, and checks that ehlo answers `/healthz`, that `up{service="ehlo"}` is 1, that every ehlo rule has health `ok`, that Grafana lists every dashboard, and that a webhook returns 401 without the token and 202 with it, first with the token from `webhookSecret`, then from `existingConfigSecret`. The values file pins every image the install pulls by digest.

## Rules and dashboards

The chart carries the rules and dashboards that `ehlo-monitoring` renders with its default config: no extra selector, rule label `component="ehlo"`. They sit in `deploy/helm/ehlo/files`, and `just dashboard` refreshes them with the committed copies in `prometheus/rules` and `grafana/dashboards`. The chart renders the same objects as `ehlo-monitoring render -format prometheusrule`, `configmap` and `grafanamanifest`: one PrometheusRule with the recording group before the alerts group, ConfigMaps named `<release>-<dashboard>-dashboard` (`ehlo-overview-dashboard` for a release named `ehlo`), and GrafanaManifest objects named `<release>-<dashboard>`. `just chart` fails when they differ in anything but labels and namespace. The chart reads them with `.Files.Get`, so `{{ $labels }}` in alert annotations reaches Prometheus unchanged.

For another selector, datasource, label set or namespace, leave `prometheusRule`, `dashboards` and `grafanaManifests` off and apply your own render. [monitoring.md](monitoring.md) lists the flags.

```
ehlo-monitoring render -selector 'namespace="mail"' -namespace mail -labels release=kps -format all -out out/
kubectl apply -f out/kubernetes/prometheusrule.yaml -f out/kubernetes/dashboards-configmaps.yaml
```

## Grafana Operator

The Grafana Operator imports schema v2 dashboards through `GrafanaManifest`, added in v5.22.0. Use v5.25 or later: v5.25.0 fixed updates of an existing `GrafanaManifest` (grafana-operator#2865) and its deletion (grafana-operator#2910). The chart creates one per dashboard:

```yaml
grafanaManifests:
  enabled: true
  instanceSelector:
    matchLabels:
      dashboards: grafana
  folderUID: ehlo        # optional, sets the grafana.app/folder annotation
```

`ehlo-monitoring render -format grafanamanifest` writes the same objects to `kubernetes/grafana-manifests.yaml` for use without the chart.

## Mimir

`ehlo-monitoring render -format mimir` writes `mimir/ehlo.yaml`, a mimirtool rule file with the namespace `ehlo`. Load it with mimirtool 3.2.5:

```
mimirtool rules sync --address=https://mimir.example.internal --id=<tenant> mimir/ehlo.yaml
```

Without a local mimirtool: `docker run --rm -v "$PWD/mimir:/rules:ro" grafana/mimirtool:3.2.5@sha256:817b078c653c4d9705496cba281ff5596bf0d5c1d46129bced583e834090b18b rules sync --address=... --id=<tenant> /rules/ehlo.yaml`.

## Values

| Key | Default | Purpose |
|-----|---------|---------|
| `image.repository`, `image.tag`, `image.digest` | `ghcr.io/cauebraz/ehlo`, appVersion, release digest | Image reference, rendered as `repository:tag@digest` |
| `config` | `sources: []`, `targets.default: 0` | `ehlo.yaml` |
| `existingConfigSecret`, `existingConfigSecretKey` | `""`, `ehlo.yaml` | Secret with the whole config, replaces `config` |
| `webhookSecret.name`, `.tokenKey`, `.sendgridPublicKeyKey` | `""`, `token`, `""` | Secret mounted at `/etc/ehlo-secrets`; sets `webhooks.token_file` and `webhooks.sendgrid_public_key_file`; not allowed with `existingConfigSecret` |
| `persistence.enabled`, `.size`, `.storageClassName`, `.existingClaim` | `true`, `1Gi` | Volume for `/data` |
| `resources` | 50m CPU and 64Mi requested, 256Mi memory limit | |
| `serviceMonitor.enabled`, `podMonitor.enabled` | `false` | Enable one; the chart fails when both are on |
| `serviceMonitor.labels`, `.job`, `.interval` | `{}`, `""`, `15s` | Selector labels for Prometheus, an optional `job` label, scrape interval |
| `prometheusRule.enabled`, `.labels` | `false`, `{}` | Recording and alerting rules |
| `dashboards.enabled`, `.label`, `.labelValue`, `.annotations`, `.namespace` | `false`, `grafana_dashboard`, `"1"` | One ConfigMap per dashboard |
| `grafanaManifests.enabled`, `.instanceSelector`, `.folderUID`, `.allowCrossNamespaceImport` | `false`, `dashboards: grafana`, `""`, `false` | One GrafanaManifest per dashboard |

The pod runs as UID 65532 with a read-only root filesystem, no capabilities, no privilege escalation and the `RuntimeDefault` seccomp profile, and does not mount a service account token. [values.yaml](../deploy/helm/ehlo/values.yaml) has every key.

## Grafana 12 and grafana.com

Each release attaches `ehlo-dashboards-classic_<version>.tar.gz`: the dashboards as classic JSON (`schemaVersion` 42) with `__inputs` and `__requires`, for Grafana 12.4 and grafana.com. `scripts/export-classic.sh` (`just classic`) builds them: a throwaway Grafana 13.3.1 provisions the v2 files, the JSON is read back through `/apis/dashboard.grafana.app/v1`, and each file is imported through `/api/dashboards/import` into Grafana 12.4.15 and 13.3.1. The import asks for a Prometheus data source (`DS_PROMETHEUS`), which becomes the default of the `datasource` variable.

The classic copy has every panel, query and description, and loses:

- auto grids: panels get fixed positions and do not reflow on narrow screens,
- panel subtitles,
- conditional rendering: the Overview open alerts table shows even when no alert is open.
