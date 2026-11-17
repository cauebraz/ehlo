#!/usr/bin/env bash
# Usage: deploy/scripts/chart-check.sh
# Lints the Helm chart, renders it with each values file in
# deploy/helm/ci/, validates the output with kubeconform against pinned
# Kubernetes and CRD schemas, checks that deploy/helm/ehlo/files matches
# the committed monitoring files, that the PrometheusRule, dashboard
# ConfigMaps and GrafanaManifest objects equal ehlo-monitoring render output
# (chart labels and namespace aside), and that three inputs fail to render:
# webhookSecret with existingConfigSecret, the checkout without image.tag,
# and values that values.schema.json rejects.
set -euo pipefail
cd "$(dirname "$0")/../.."

chart=deploy/helm/ehlo
helm=$(deploy/scripts/helm.sh path)
tag=(--set image.tag=ci)

kube_versions=(1.30.0 1.37.0)
k8s_schemas=6ea44f54bb5cdedb2f61fba42c9b342e97959262
crd_schemas=3e8a8916ca1b7af6464606c2c2f5869dbafa9653

step() { printf '\n== %s\n' "$1"; }

step "chart files match the committed monitoring files"
deploy/scripts/chart-files.sh -check

step "helm lint"
"$helm" lint "$chart" --strict "${tag[@]}"
for f in deploy/helm/ci/*.yaml; do
    "$helm" lint "$chart" --strict "${tag[@]}" -f "$f"
done

step "helm template | kubeconform"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
go build -modfile=tools/kubeconform/go.mod -o "$out/kubeconform" github.com/yannh/kubeconform/cmd/kubeconform
for f in deploy/helm/ci/*.yaml; do
    "$helm" template ehlo "$chart" "${tag[@]}" --namespace ehlo -f "$f" > "$out/$(basename "$f")"
    for v in "${kube_versions[@]}"; do
        "$out/kubeconform" -strict -summary -kubernetes-version "$v" \
            -schema-location "https://raw.githubusercontent.com/yannh/kubernetes-json-schema/$k8s_schemas/{{.NormalizedKubernetesVersion}}-standalone{{.StrictSuffix}}/{{.ResourceKind}}{{.KindSuffix}}.json" \
            -schema-location "https://raw.githubusercontent.com/datreeio/CRDs-catalog/$crd_schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" \
            "$out/$(basename "$f")"
    done
done

step "rules reach the PrometheusRule without template evaluation"
"$helm" template ehlo "$chart" "${tag[@]}" --set prometheusRule.enabled=true --show-only templates/prometheusrule.yaml | grep -q '\$labels' \
    || { echo "PrometheusRule lost {{ \$labels }}"; exit 1; }

step "chart objects match ehlo-monitoring render"
go build -modfile=tools/yq/go.mod -o "$out/yq" github.com/mikefarah/yq/v4
go build -o "$out/ehlo-monitoring" ./cmd/ehlo-monitoring
normalize='[.] | map(select(.kind == "PrometheusRule" or .kind == "GrafanaManifest" or (.kind == "ConfigMap" and .metadata.labels.grafana_dashboard == "1")))
    | map(del(.metadata.namespace) | .metadata.labels = ((.metadata.labels // {}) | with_entries(select(.key | test("^(app[.]kubernetes[.]io|helm[.]sh)/") | not))))
    | map(with(select(.metadata.labels == {}); del(.metadata.labels)) | sort_keys(..) | ... comments = "" | ... style = "")
    | sort_by(.kind, .metadata.name) | .[] | split_doc'
yaml11_words='.. | select(tag == "!!str" and style == "" and test("^(y|Y|yes|Yes|YES|n|N|no|No|NO|on|On|ON|off|Off|OFF)$")) | path | join(".")'
for format in prometheusrule configmap grafanamanifest; do
    "$out/ehlo-monitoring" render -format "$format" -out "$out/render-$format" >/dev/null
    yaml11=$("$out/yq" ea -N "$yaml11_words" "$out"/render-"$format"/kubernetes/*.yaml | sort -u)
    [[ -z "$yaml11" ]] || { printf 'ehlo-monitoring render -format %s writes unquoted strings that kubectl and Helm read as booleans:\n%s\n' "$format" "$yaml11"; exit 1; }
    "$out/yq" ea -N "$normalize" "$out"/render-"$format"/kubernetes/*.yaml > "$out/want-$format.yaml"
    case $format in
        prometheusrule) show=templates/prometheusrule.yaml ;;
        configmap) show=templates/dashboards.yaml ;;
        grafanamanifest) show=templates/grafanamanifests.yaml ;;
    esac
    "$helm" template ehlo "$chart" "${tag[@]}" --namespace ehlo --set prometheusRule.enabled=true --set dashboards.enabled=true \
        --set grafanaManifests.enabled=true --show-only "$show" | "$out/yq" ea -N "$normalize" - > "$out/got-$format.yaml"
    [[ -s "$out/want-$format.yaml" ]] || { echo "ehlo-monitoring render -format $format wrote no objects"; exit 1; }
    diff -u "$out/want-$format.yaml" "$out/got-$format.yaml" >"$out/diff-$format" \
        || { head -50 "$out/diff-$format"; echo "chart $show differs from ehlo-monitoring render -format $format"; exit 1; }
    echo "$format: $(grep -c '^kind:' "$out/got-$format.yaml") objects match"
done

step "webhookSecret with existingConfigSecret fails"
if err=$("$helm" template ehlo "$chart" "${tag[@]}" --set existingConfigSecret=ehlo-config --set webhookSecret.name=ehlo-webhooks 2>&1); then
    echo "rendering with existingConfigSecret and webhookSecret.name succeeded, want a failure"; exit 1
fi
grep -q 'webhookSecret does not apply with existingConfigSecret' <<<"$err" || { echo "unexpected error: $err"; exit 1; }
echo "fails as expected"

step "a checkout without image.tag fails"
if err=$("$helm" template ehlo "$chart" 2>&1); then
    echo "rendering the checkout without image.tag succeeded, want a failure"; exit 1
fi
grep -q 'image.tag is required' <<<"$err" || { echo "unexpected error: $err"; exit 1; }
echo "fails as expected"

step "values.schema.json rejects unknown keys and wrong types"
for bad in serviceMonitor.enable=true service.port=http persistence.enabled=yes 'image.digest=sha256:abc'; do
    if err=$("$helm" template ehlo "$chart" "${tag[@]}" --set-string "$bad" 2>&1); then
        echo "--set-string $bad rendered, want a schema error"; exit 1
    fi
    grep -q 'specifications of the schema' <<<"$err" || { echo "--set-string $bad: unexpected error: $err"; exit 1; }
done
echo "rejected as expected"

printf '\nchart checks passed\n'
