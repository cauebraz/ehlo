set shell := ["bash", "-euo", "pipefail", "-c"]

scenarios := "steady warmup over-capacity gmail-throttle connect-refused dkim-down complaint-spike unknown-source"

default:
    @just --list

# Create .env with a random Grafana admin password, once
setup:
    @if [[ ! -f .env ]]; then printf 'GRAFANA_ADMIN_PASSWORD=%s\n' "$(openssl rand -hex 16)" > .env && echo "created .env"; else echo ".env exists"; fi

# Format Go code
fmt:
    gofmt -w .

# The lint job CI runs: golangci-lint, govulncheck, actionlint, goreleaser check, at the versions in tools/
lint:
    go tool -modfile=tools/golangci-lint/go.mod golangci-lint run
    go tool -modfile=tools/govulncheck/go.mod govulncheck ./...
    go tool -modfile=tools/actionlint/go.mod actionlint
    go tool -modfile=tools/goreleaser/go.mod goreleaser check

# List the pins Dependabot does not update (syft, kind, kubectl, Helm, e2e charts and images) next to the newest upstream release
outdated:
    scripts/outdated.sh

# Build the binaries into bin/
build:
    mkdir -p bin && go build -o bin/ ./cmd/...

# Regenerate grafana/dashboards and prometheus/rules from monitoring/, and copy them into the Helm chart
dashboard:
    go run ./cmd/ehlo-monitoring update
    deploy/scripts/chart-files.sh

# Lint the Helm chart, render it and validate it with kubeconform
chart:
    deploy/scripts/chart-check.sh

# Install kube-prometheus-stack and the chart on a throwaway kind cluster and check scrape, rules and dashboards
chart-e2e:
    deploy/scripts/kind-e2e.sh

# Write classic (schema v1) copies of the dashboards to dist/classic and check they import into Grafana 12.4 and 13.3
classic:
    scripts/export-classic.sh

# Render every dashboard to docs/images/dashboards from a throwaway stack: steady for 10 min, then the scenario for 20 min
screenshots scenario="gmail-throttle":
    scripts/screenshots.sh -scenario {{scenario}}

# The check job CI runs: gofmt, tidy, vet, race tests, generated dashboard, promtool, compose config
check:
    ./scripts/check.sh

# Start the stack (server, simulator, Prometheus, Grafana) with the scenario in .env and wait until healthy
up: setup
    docker compose up -d --build --wait
    docker compose kill -s HUP prometheus

# Restart the simulator with another scenario, kept in .env so later ups keep it
scenario name speed="60": setup
    @if [[ " {{scenarios}} " != *" {{name}} "* ]]; then echo "unknown scenario {{name}}, want one of: {{scenarios}}"; exit 1; fi
    @grep -v '^EHLO_' .env > .env.next && printf 'EHLO_SCENARIO={{name}}\nEHLO_SPEED={{speed}}\n' >> .env.next && mv .env.next .env
    docker compose up -d --wait ehlo-sim

# Reload Prometheus rules and config without a restart
reload:
    docker compose kill -s HUP prometheus

# Query every dashboard panel, the limits API and the rule health against the running stack
smoke:
    ./scripts/smoke.sh

# Stop the stack, keep the data volumes
down:
    docker compose down

# Stop the stack, delete every volume (warmup state included) and go back to the steady scenario
reset:
    docker compose down --volumes
    @if [[ -f .env ]]; then grep -v '^EHLO_' .env > .env.next && mv .env.next .env; fi

ps:
    docker compose ps

logs service="":
    docker compose logs --tail 100 {{service}}
