# Contributing

## Setup

Requires Go 1.27 or later, Docker with Compose v2, [just](https://github.com/casey/just), and [golangci-lint](https://golangci-lint.run) v2.15.0 for `just lint`.

```
just check    # the CI check job: format, tidy, vet, race tests, generated dashboards, rule tests, compose config
just lint     # the CI lint job: golangci-lint, govulncheck, actionlint, goreleaser check
just up       # full stack
just smoke    # the CI smoke job: every dashboard panel returns data
just chart    # the CI chart job: helm lint, template and kubeconform
just chart-e2e  # the chart on a throwaway kind cluster with kube-prometheus-stack
just classic  # classic (v1) dashboards, imported into Grafana 12.4 and 13.3
```

Run `just check` and `just lint` before a pull request. If you changed the server, the simulator, a panel or a rule, also run `just up` and `just smoke`.

CI also runs CodeQL, dependency review on pull requests, and OpenSSF Scorecard on `main`.

## Layout

```
cmd/ehlo           server and Postfix tailer
cmd/ehlo-sim       traffic simulator
cmd/ehlo-monitoring renders the dashboards and rules; update writes grafana/dashboards and prometheus/rules
internal/config    config file and defaults
internal/event     event format and validation
internal/provider  recipient domain to provider
internal/classify  SMTP reply categories
internal/warmup    warmup policy and lanes
internal/metrics   Prometheus metrics
internal/ingest    classify, validate, record: the path of every event
internal/server    HTTP handlers
internal/client    HTTP client for the tailer and the simulator
internal/adapters  Postfix, SES, SendGrid
internal/sim       traffic generator behind ehlo-sim
monitoring         Grafana dashboards and Prometheus rules, written in Go (public package, API v0)
prometheus/rules   generated recording rules and alerts
prometheus/tests   promtool tests of the default render, a -group-by cluster render, and two renders sharing one Prometheus
```

## Common changes

| Change | Steps |
|--------|-------|
| Metric | Add it in `internal/metrics` and to `docs/metrics.md`. Labels take a small, fixed set of values: never a recipient, message ID or domain |
| Panel | Add it in `monitoring` (`dashboard.go`, `providers.go` or `codes.go`), then `just dashboard`. Never edit the JSON. Tests fail if a panel queries a metric that does not exist, if a title or subtitle does not fit the narrowest column its auto grid allows, or if a gauge is not taken once per source or lane (`max by`) |
| Alert | Add it to `monitoring/rules.go`, then `just dashboard`. Add a test case to `prometheus/tests/ehlo-alerts.test.yml` and a section to `docs/alerts.md` |
| SMTP reply pattern | Add it to `internal/classify` with a test using the provider's real reply text. New token format: a row in `docs/metrics.md`. Code shown on a chart: an entry in `codeNotes` in `monitoring/codes.go` |
| Warmup policy key | Add it to `internal/warmup/policy.go` with a default in `DefaultPolicy`, a validation rule, a test, and a row in `docs/limits.md` |

The config file is decoded over the defaults, so a key set to zero stays zero. Day-close rules are one ordered `verdict` in `internal/warmup/day.go`; a new rule is a new case plus a row in `verdict_test.go`.

## Style

- Standard Go. Wrap errors with `%w`. Every goroutine has a way to stop.
- Keep dependencies few. Today there are two direct ones: the Prometheus client and a YAML parser.
- Example IPs come from `203.0.113.0/24`. No real company, customer or IP in code, tests or docs.
- Pin every external reference: actions by commit SHA with the version in a comment, images by tag and digest, `go run` tools by exact version. Dependabot updates them weekly.
- Commit titles follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/): `<type>(<scope>): <summary>`, for example `fix(postfix): parse long queue ids`. Types: `feat`, `fix`, `docs`, `chore`, `refactor`, `test`, `ci`, `build`, `perf`, `revert`. A breaking change adds `!` after the scope.
- User-visible changes get a line in `CHANGELOG.md` under Unreleased.

## Releases

1. Move the Unreleased notes in `CHANGELOG.md` under the new version and date.
2. Push a tag such as `v0.2.0` from `main`.
3. The release workflow runs `scripts/check.sh`, pushes the multi-arch image to GHCR, builds the Helm chart (image pinned by digest), the monitoring bundle and the classic dashboards, builds archives, SBOMs and `checksums.txt` into a draft GitHub release, then pushes the chart to `oci://ghcr.io/cauebraz/charts` (the image goes first because the chart pins its digest), and attests the files, the image and the chart with GitHub build provenance.
4. Check the artifacts, then publish the draft.

## Security

Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md).
