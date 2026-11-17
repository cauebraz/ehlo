#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

prom="${PROM_URL:-http://127.0.0.1:9090}"
grafana="${GRAFANA_URL:-http://127.0.0.1:3000}"
ehlo="${EHLO_URL:-http://127.0.0.1:8080}"
failures=0

fail() { printf 'FAIL %s\n' "$1"; failures=$((failures + 1)); }
query() { curl -fsS "$prom/api/v1/query" --data-urlencode "query=$1" | jq '.data.result | length'; }

summary() {
    if (( failures > 0 )); then
        printf '\n%d smoke failures\n' "$failures"
        exit 1
    fi
    printf '\nsmoke passed\n'
}

if curl -fsS "$ehlo/healthz" >/dev/null; then
    if lanes=$(curl -fsS "$ehlo/v1/limits" | jq -e '.lanes | length'); then
        (( lanes > 0 )) || fail "limits api lists no lanes"
        printf 'limits api: %s lanes\n' "$lanes"
    else
        fail "limits api"
    fi
else
    fail "ehlo health at $ehlo"
fi

if curl -fsS "$grafana/api/health" >/dev/null; then
    for file in grafana/dashboards/*.json; do
        uid=$(jq -r .metadata.name "$file")
        if title=$(curl -fsS "$grafana/apis/dashboard.grafana.app/v2/namespaces/default/dashboards/$uid" | jq -er '.spec.title'); then
            printf 'grafana dashboard: %s\n' "$title"
        else
            fail "grafana dashboard $uid"
        fi
    done
else
    fail "grafana health at $grafana; dashboards not checked"
fi

if ! curl -fsS "$prom/-/ready" >/dev/null; then
    fail "prometheus not ready at $prom; scrape, rules, panel queries, variables and annotations not checked"
    summary
fi

up=$(curl -fsS "$prom/api/v1/query" --data-urlencode 'query=min(up and on (job, instance) ehlo_build_info)' | jq -r '.data.result[0].value[1] // "missing"') || up=error
[[ "$up" == "1" ]] || fail "ehlo scrape target up=$up"

bad_rules=$(curl -fsS "$prom/api/v1/rules" | jq -r '[.data.groups[].rules[] | select(.health != "ok") | .name] | join(", ")') || bad_rules="rules api failed"
[[ -z "$bad_rules" ]] || fail "rules not healthy: $bad_rules"
unlabeled=$(curl -fsS "$prom/api/v1/rules?type=alert" | jq -r '[.data.groups[].rules[] | select(.labels.component != "ehlo") | .name] | join(", ")') || unlabeled="rules api failed"
[[ -z "$unlabeled" ]] || fail "alerts without component=ehlo, hidden from the dashboards: $unlabeled"

while IFS=$'\t' read -r panel expr; do
    q=${expr//\$__rate_interval/1m}
    q=${q//\$__range/1h}
    q=${q//\$source/.*}
    q=${q//\$provider/.*}
    n=$(query "$q") || { fail "$panel query failed"; continue; }
    if [[ "$expr" == *"or vector(0)"* ]]; then
        while read -r metric; do
            [[ "$(query "count($metric)")" == "1" ]] || fail "$panel reads $metric, which has no series"
        done < <(grep -oE 'ehlo_[a-z0-9_]+' <<<"$expr" | sort -u)
    fi
    if [[ "$n" == "0" ]]; then
        if [[ "$expr" == *ALERTS* ]]; then
            printf 'ok   %-52s no firing alerts\n' "$panel"
        else
            fail "$panel returned no series"
        fi
    else
        printf 'ok   %-52s %s series\n' "$panel" "$n"
    fi
done < <(jq -r '.metadata.name as $d | .spec.elements[].spec | ($d + ": " + .title) as $t | .data.spec.queries[].spec.query.spec | [$t, (.expr | gsub("\n"; " "))] | @tsv' grafana/dashboards/*.json)

while IFS= read -r def; do
    if [[ "$def" =~ ^label_values\((.+),\ *([a-z_]+)\)$ ]]; then
        n=$(curl -fsS -G "$prom/api/v1/label/${BASH_REMATCH[2]}/values" --data-urlencode "match[]=${BASH_REMATCH[1]}" | jq '.data | length') || n=0
        if (( n > 0 )); then
            printf 'ok   %-52s %s values\n' "variable $def" "$n"
        else
            fail "variable $def has no values"
        fi
    else
        fail "variable query is not label_values: $def"
    fi
done < <(jq -r '.spec.variables[] | select(.kind == "QueryVariable") | .spec.query.spec.query' grafana/dashboards/*.json | sort -u)

while IFS= read -r expr; do
    if n=$(query "$expr"); then
        printf 'ok   %-52s %s series\n' "annotation $expr" "$n"
    else
        fail "annotation $expr"
    fi
done < <(jq -r '.spec.annotations[].spec.legacyOptions.expr' grafana/dashboards/*.json | sort -u)

summary
