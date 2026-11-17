#!/usr/bin/env bash
# Usage: scripts/outdated.sh
# Lists the pins Dependabot does not update and the newest upstream release
# of each: syft, kind, kubectl, Helm, the kind node image, the
# kube-prometheus-stack and Grafana Operator charts, and the copies of the
# compose.yaml Grafana image in scripts and the kind e2e. Dependabot covers
# go.mod, tools/*/go.mod, actions, the Dockerfile and compose.yaml. Writes a
# Markdown table to stdout, and to GITHUB_STEP_SUMMARY when set. Exits 0;
# a pin that is behind prints a ::warning:: line in GitHub Actions. Set
# GITHUB_TOKEN to raise the GitHub API rate limit. Needs curl and jq.
set -euo pipefail
cd "$(dirname "$0")/.."

gh_api() {
    local auth=()
    [[ -n "${GITHUB_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
    curl -fsSL ${auth[@]+"${auth[@]}"} -H "Accept: application/vnd.github+json" "https://api.github.com/$1"
}
gh_latest() { gh_api "repos/$1/releases/latest" | jq -r .tag_name; }
ghcr_latest() {
    local token last="" page tags=""
    token=$(curl -fsSL "https://ghcr.io/token?scope=repository:$1:pull" | jq -r .token)
    while :; do
        page=$(curl -fsSL -H "Authorization: Bearer $token" "https://ghcr.io/v2/$1/tags/list?n=1000&last=$last" | jq -r '.tags // [] | .[]')
        [[ -n "$page" ]] || break
        tags+="$page"$'\n'
        [[ "$(tail -1 <<<"$page")" != "$last" ]] || break
        last=$(tail -1 <<<"$page")
    done
    grep -E '^v?[0-9]+\.[0-9]+\.[0-9]+$' <<<"$tags" | sort -V | tail -1
}
hub_latest() {
    curl -fsSL "https://hub.docker.com/v2/repositories/$1/tags?page_size=100&ordering=last_updated" \
        | jq -r '.results[].name | select(test("^v?[0-9]+[.][0-9]+[.][0-9]+$"))' | sort -V | tail -1
}
pin() { sed -n "s|$2|\\1|p" "$1" | head -1; }

compose_grafana=$(pin compose.yaml '^ *image: *grafana/grafana:\([0-9.]*\)@.*$')

rows=()
behind=0
row() {
    local name=$1 file=$2 current=$3 latest=$4 status=ok
    if [[ -z "$current" ]]; then
        status="pin not found"
    elif [[ -z "$latest" ]]; then
        status="upstream lookup failed"
    elif [[ "${current#v}" != "${latest#v}" ]]; then
        status=behind
        behind=$((behind + 1))
        [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::warning file=$file::$name $current is behind $latest"
    fi
    rows+=("| $name | \`$file\` | $current | $latest | $status |")
}
try() { "$@" 2>/dev/null || true; }

row syft .github/workflows/release.yml \
    "$(pin .github/workflows/release.yml '^  SYFT_VERSION: \(.*\)$')" "$(try gh_latest anchore/syft)"
row kind .github/workflows/chart-e2e.yml \
    "$(pin .github/workflows/chart-e2e.yml '^  KIND_VERSION: \(.*\)$')" "$(try gh_latest kubernetes-sigs/kind)"
row kubectl .github/workflows/chart-e2e.yml \
    "$(pin .github/workflows/chart-e2e.yml '^  KUBECTL_VERSION: \(.*\)$')" "$(try curl -fsSL https://dl.k8s.io/release/stable.txt)"
row helm deploy/scripts/helm.sh \
    "$(pin deploy/scripts/helm.sh '^version=\(.*\)$')" "$(try gh_latest helm/helm)"
row kindest/node deploy/scripts/kind-e2e.sh \
    "$(pin deploy/scripts/kind-e2e.sh '^node_image=kindest/node:\([^@]*\)@.*$')" "$(try hub_latest kindest/node)"
row kube-prometheus-stack deploy/scripts/kind-e2e.sh \
    "$(pin deploy/scripts/kind-e2e.sh '^kps_version=\(.*\)$')" "$(try ghcr_latest prometheus-community/charts/kube-prometheus-stack)"
row grafana-operator deploy/scripts/kind-e2e.sh \
    "$(pin deploy/scripts/kind-e2e.sh '^gop_version=\(.*\)$')" "$(try gh_latest grafana/grafana-operator)"
row "grafana (compose.yaml)" deploy/scripts/kind-e2e.sh \
    "$(pin deploy/scripts/kind-e2e.sh '^grafana_image=docker.io/grafana/grafana:\([^@]*\)@.*$')" "$compose_grafana"
row "grafana (compose.yaml)" scripts/export-classic.sh \
    "$(pin scripts/export-classic.sh '^export_image=grafana/grafana:\([^@]*\)@.*$')" "$compose_grafana"
row "grafana (compose.yaml)" deploy/kind/kube-prometheus-stack.yaml \
    "$(sed -n '/repository: grafana\/grafana$/{n;s/^ *tag: *//p;}' deploy/kind/kube-prometheus-stack.yaml)" "$compose_grafana"

table=$(
    echo "| Pin | File | Current | Latest | Status |"
    echo "|-----|------|---------|--------|--------|"
    printf '%s\n' "${rows[@]}"
    echo
    echo "$behind of ${#rows[@]} pins behind. Grafana copies follow compose.yaml, which Dependabot updates. The kube-prometheus-stack image pins in deploy/kind/kube-prometheus-stack.yaml follow the chart defaults."
)
echo "$table"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then echo "$table" >> "$GITHUB_STEP_SUMMARY"; fi
