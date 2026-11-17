# Monitoring on your stack

`grafana/dashboards/` and `prometheus/rules/` hold the default render: no selector, no `group_by` labels, uid prefix `ehlo-`, and the `ALERTS` series of Prometheus. `ehlo-monitoring render` writes the same dashboards and rules for another stack, so you do not edit generated files or fork. The generator is the Go package `github.com/cauebraz/ehlo/monitoring` (API v0, unstable until v1).

```
go run github.com/cauebraz/ehlo/cmd/ehlo-monitoring@<tag> render -config monitoring.yaml -format all -out monitoring
```

`render` refuses to leave ehlo dashboards it did not write in `<out>/dashboards`, such as those of an earlier `uid_prefix`; `-clean` removes them. Other files there stay.

## Options

Each key has a flag; a flag overrides the config file.

| Key | Flag | Default | Effect |
|-----|------|---------|--------|
| `selector` | `-selector` | empty | Label matchers added to every `ehlo_` selector in rules and dashboards, such as `job="mail-ehlo", namespace="mail"`. Use target labels, which every series carries. The labels of equality (`=`) matchers are kept by every recording rule and alert, and the rules and dashboards read only recorded series and alerts that carry them |
| `group_by` | `-group-by` | empty | Labels such as `cluster,namespace` kept by every `by` and `on` clause of the rules and dashboards. Each becomes a multi-select dashboard filter, and chart legends start with it. Labels of ehlo's series (`source`, `class`, ...) and of alerts (`severity`, `alertname`, ...) are rejected |
| `datasource` | `-datasource` | empty | Datasource uid the dashboards select first. Empty selects Grafana's default datasource |
| `uid_prefix` | `-uid-prefix` | `ehlo-` | Dashboard uid and file name prefix; links between dashboards follow it. Lowercase letters, digits and `-`. Without trailing hyphens it also names the rule groups (`<name>-recording`, `<name>-alerts`), the Mimir namespace and the PrometheusRule |
| `alerts_metric` | `-alerts-metric` | `ALERTS` | Alert state series the dashboards read: `ALERTS` or `GRAFANA_ALERTS` |
| `rule_interval` | `-rule-interval` | `15s` | Evaluation interval of both rule groups |
| `rule_labels` | `-rule-labels` | empty | Extra labels on every alert, such as `team=mail`. Every alert always carries `component: ehlo` and `severity`. ehlo's labels and the labels kept from `group_by` and the selector are rejected |
| `runbook_url` | `-runbook-url` | `https://github.com/cauebraz/ehlo/blob/main/docs/alerts.md#{{alert}}` | `runbook_url` annotation; `{{alert}}` becomes the lowercased alert name |
| `dashboard_url` | `-dashboard-url` | empty | Grafana base URL, path and query string kept, such as `https://grafana.example.com/grafana?orgId=2`. When set, each alert gets an absolute `dashboard_url` annotation. Every alert has `dashboard_uid` |
| `folder_uid` | `-folder-uid` | empty | Grafana folder of the GrafanaManifest objects |
| `kubernetes.namespace` | `-namespace` | empty | Namespace of the Kubernetes objects, a DNS-1123 label |
| `kubernetes.labels` | `-labels` | empty | Labels of the Kubernetes objects, such as `release=kube-prometheus-stack` |
| `kubernetes.annotations` | `-annotations` | empty | Annotations of the Kubernetes objects, such as `grafana_folder=Email` for the dashboard sidecar |
| `kubernetes.instance_selector` | `-instance-selector` | `dashboards=grafana` | Grafana instances the GrafanaManifest objects select. `grafanamanifest` refuses an empty one, which selects every instance |
| `configmap_label` | `-configmap-label` | `grafana_dashboard=1` | Label the Grafana dashboard sidecar watches |

Label and annotation keys and label values follow the Kubernetes rules.

`-format` picks the output; `all` writes every one:

| Format | Path under `-out` | For |
|--------|-------------------|-----|
| `grafana` | `dashboards/<uid>.json` | Grafana file provisioning, the v2 API, Terraform |
| `prometheus` | `rules/ehlo-recording.yml`, `rules/ehlo-alerts.yml` | Prometheus `rule_files`, Thanos Ruler |
| `mimir` | `mimir/ehlo.yaml` | `mimirtool rules load`, namespace `ehlo` (the uid prefix without trailing hyphens) |
| `prometheusrule` | `kubernetes/prometheusrule.yaml` | prometheus-operator, `monitoring.coreos.com/v1` |
| `configmap` | `kubernetes/dashboards-configmaps.yaml` | Grafana dashboard sidecar, one ConfigMap per dashboard, key `<uid>.json` |
| `grafanamanifest` | `kubernetes/grafana-manifests.yaml` | Grafana Operator `GrafanaManifest`, one per dashboard |

`ehlo-monitoring check` fails when the committed files differ from the default render, and `ehlo-monitoring update` rewrites them.

## Examples

Plain Prometheus scraping ehlo as job `mail-ehlo`:

```
ehlo-monitoring render -selector 'job="mail-ehlo"' -format prometheus -out monitoring
```

Add the files under `rule_files` and reload Prometheus.

kube-prometheus-stack, with a ServiceMonitor whose Service is named `mail-ehlo`:

```
selector: job="mail-ehlo", namespace="mail"
kubernetes:
  namespace: monitoring
  labels:
    release: kube-prometheus-stack
```

```
ehlo-monitoring render -config monitoring.yaml -format all -out monitoring
kubectl apply --server-side -f monitoring/kubernetes/prometheusrule.yaml -f monitoring/kubernetes/dashboards-configmaps.yaml
```

The chart selects PrometheusRules and the sidecar selects ConfigMaps by label; both labels above match its defaults. The sidecar reads only keys that end in `.json`.

Mimir or Grafana Cloud with several clusters in one tenant:

```
selector: job="mail-ehlo"
group_by: [cluster]
datasource: mimir-prod
dashboard_url: https://grafana.example.com
```

```
ehlo-monitoring render -config monitoring.yaml -format all -out monitoring
mimirtool rules load monitoring/mimir/ehlo.yaml
```

Recording rules and alerts keep `cluster`, so Alertmanager can route on it, and the dashboards gain a cluster filter.

Two renders in one Prometheus, such as production and staging:

```
ehlo-monitoring render -selector 'job="mail-ehlo"' -format all -out prod
ehlo-monitoring render -selector 'job="ehlo-staging"' -uid-prefix staging- -format all -out staging
```

Each render's recorded series and alerts keep its `job`, and its dashboards and alerts read only those. The distinct `uid_prefix` keeps the rule groups, the Mimir namespace, the PrometheusRule and the dashboard uids apart. A regex or negative matcher does not scope the render.

Grafana-managed alerts: Grafana writes no `ALERTS` series. With the Prometheus state history backend, Grafana writes `GRAFANA_ALERTS` to a Prometheus data source through remote write ([Grafana: configure alert state history](https://grafana.com/docs/grafana/latest/alerting/set-up/configure-alert-state-history/)):

```
[unified_alerting.state_history]
enabled = true
backend = prometheus
prometheus_target_datasource_uid = <uid of the Prometheus data source>
```

Start Prometheus with `--web.enable-remote-write-receiver`, and set the data source's Prometheus type to match the server. Render with `alerts_metric: GRAFANA_ALERTS`; Email health, Open alerts and the alert annotations then read it. To import the rules as Grafana-managed rules, load `mimir/ehlo.yaml` with `mimirtool rules load` against Grafana's `/api/convert/` endpoint.

## Importing the dashboards

The dashboards use the Grafana dashboard schema v2 and need Grafana 13 or later. Load them with a file provider, the sidecar, the Grafana Operator (`GrafanaManifest`; `GrafanaDashboard` accepts only classic JSON), or the v2 API. On a self-hosted Grafana the namespace of org 1 is `default`; Grafana Cloud uses `stacks-<id>`.

Create, in a folder: add the folder uid to `metadata.annotations` and POST the file.

```
jq '.metadata.annotations["grafana.app/folder"] = "<folder-uid>"' monitoring/dashboards/ehlo-overview.json |
  curl -fsS -H "Authorization: Bearer $GRAFANA_TOKEN" -H 'Content-Type: application/json' \
    -X POST --data-binary @- "$GRAFANA_URL/apis/dashboard.grafana.app/v2/namespaces/default/dashboards"
```

Update: PUT to the dashboard's own path. Copy `metadata.resourceVersion` from a GET of the dashboard into the body; Grafana rejects a stale version with 409.

```
curl -fsS -X PUT ... "$GRAFANA_URL/apis/dashboard.grafana.app/v2/namespaces/default/dashboards/ehlo-overview"
```

## One replica

Run one ehlo process per warmup state. Each process holds the warmup lanes in memory and in its state file. With two replicas behind one Service, each receives part of the events, so each counts part of each lane: daily limits, backoffs and pauses diverge, and `/v1/limits` answers depend on the replica. On Kubernetes, use `replicas: 1` with the `Recreate` strategy and a volume for `/data`.

The dashboards and rules still count correctly while two processes overlap, for example during a rollout: counters are summed, and gauges such as the deferred queue and the warmup state are taken once per source or lane (`max by`). The gauges then show one replica's view, not a merged one.
