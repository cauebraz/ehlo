#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

promtool_image=$(sed -n 's|^ *image: *\(prom/prometheus:[^ ]*\) *$|\1|p' compose.yaml)
[[ "$promtool_image" == prom/prometheus:*@sha256:* ]] || { echo "compose.yaml has no digest-pinned prom/prometheus image"; exit 1; }

step() { printf '\n== %s\n' "$1"; }

step "gofmt"
unformatted=$(gofmt -l .)
if [[ -n "$unformatted" ]]; then
    printf 'not formatted:\n%s\n' "$unformatted"
    exit 1
fi

step "go mod tidy"
go mod tidy -diff

step "go vet"
go vet ./...

step "go test -race"
go test ./... -race -count=1

step "dashboards and rules match the code"
go run ./cmd/ehlo-monitoring check

step "promtool check rules"
docker run --rm -v "$PWD/prometheus:/etc/prometheus:ro" -w /etc/prometheus/rules --entrypoint promtool "$promtool_image" \
    check rules ehlo-recording.yml ehlo-alerts.yml

step "promtool check config"
docker run --rm -v "$PWD/prometheus:/etc/prometheus:ro" --entrypoint promtool "$promtool_image" \
    check config --syntax-only /etc/prometheus/prometheus.yml

step "promtool test rules"
docker run --rm -v "$PWD/prometheus:/etc/prometheus:ro" -w /etc/prometheus/tests --entrypoint promtool "$promtool_image" \
    test rules ehlo-alerts.test.yml

variant=$(mktemp -d)
trap 'rm -rf "$variant"' EXIT

step "promtool test rules rendered with -selector job=\"mail-ehlo\" -group-by cluster, next to a job=\"ehlo-staging\" render"
go run ./cmd/ehlo-monitoring render -selector 'job="mail-ehlo"' -group-by cluster -format all -out "$variant" >/dev/null
go run ./cmd/ehlo-monitoring render -selector 'job="ehlo-staging"' -group-by cluster -uid-prefix staging- -format prometheus -out "$variant/staging" >/dev/null
cp prometheus/tests/ehlo-variant.test.yml prometheus/tests/ehlo-coexist.test.yml "$variant/"
docker run --rm -v "$variant:/work:ro" -w /work --entrypoint promtool "$promtool_image" \
    test rules ehlo-variant.test.yml ehlo-coexist.test.yml

step "promtool parses every dashboard query, default and variant"
dashboard_queries() {
    jq -s '{groups: [{name: "dashboard-queries", rules: [
        [.[] | .spec | (.elements[].spec.data.spec.queries[].spec.query.spec.expr),
            (.annotations[].spec.legacyOptions.expr),
            (.variables[] | select(.kind == "QueryVariable") | .spec.query.spec.query | capture("^label_values\\((?<s>.+), *[a-z_]+\\)$").s)]
        | to_entries[]
        | {record: "dashboard:query_\(.key)", expr: (.value
            | gsub("\\$__rate_interval"; "1m") | gsub("\\$__range"; "1h") | gsub("\\$[a-z_]+"; ".*"))}
    ]}]}' "$1"/*.json
}
dashboard_queries grafana/dashboards > "$variant/queries-default.yml"
dashboard_queries "$variant/dashboards" > "$variant/queries-variant.yml"
docker run --rm -v "$variant:/work:ro" -w /work --entrypoint promtool "$promtool_image" \
    check rules --lint=none queries-default.yml queries-variant.yml

step "compose config"
GRAFANA_ADMIN_PASSWORD=check docker compose config --quiet

printf '\nall checks passed\n'
