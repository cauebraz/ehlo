# AGENTS.md

Read [CONTRIBUTING.md](CONTRIBUTING.md) first. The notes below are for coding agents.

- Run `just check` before and after a change. It needs Docker. Run `just lint` after editing Go code, `.github/workflows/` or `.goreleaser.yaml`.
- After editing `monitoring/`, run `just dashboard`. Never edit `grafana/dashboards/` or `prometheus/rules/`; both are generated.
- `docs/events.md` and `docs/metrics.md` describe the public format. Update them in the same change as the code.
- Keep ehlo generic. Do not name a company, a customer or a real IP. Example IPs come from 203.0.113.0/24.
- Pin external references: actions by commit SHA with `# vX.Y.Z`, images by `tag@sha256:` digest, `go run` tools by exact version. `scripts/check.sh` reads the Prometheus image from `compose.yaml`.
- Do not commit, push or open a pull request unless asked.
