# Contributing to Spoor

Thanks for helping. Small, focused pull requests are easiest to review.

## Getting started

```sh
make test                  # unit, golden and MCP tests; no Docker needed
make it-reset && make it   # integration tests against real backends (Docker)
```

## Where things live

| Path | What |
| --- | --- |
| `cmd/spoor` | CLI and MCP server entry point |
| `internal/digest` | Scorers, attribution, ranking, budgeting: most changes land here |
| `internal/server` | MCP tool definitions, HTTP handler, auth, telemetry hooks |
| `internal/backend` | Read-only clients for VictoriaMetrics and LogsQL |
| `internal/config` | Config schema and defaults (update `examples/spoor.yaml` and `docs/configuration.md` with it) |
| `internal/gen`, `cmd/spoorgen` | Synthetic incidents with ground truth |
| `test/integration` | Tests against real backends (build tag `integration`) |
| `bench/` | Agent benchmark harness |
| `deploy/` | Kubernetes, Docker, systemd, OpenTelemetry Demo and test-backend files |
| `docs/` | User documentation |

[docs/architecture.md](docs/architecture.md) explains how the pieces fit.

## Ground rules

- **Spoor stays read-only.** It only calls query endpoints; `TestReadOnly` enforces this.
- **No LLM calls inside Spoor.** Output must be deterministic so it can be tested byte for byte.
- **Golden files change on purpose.** If your change alters any tool output, refresh with
  `go test ./internal/digest -run TestGolden -update` and explain the difference in the PR.
- **Scoring changes need evidence.** Re-run `make it` and keep the ground-truth service ranked first in
  every fault scenario, with no findings in the control scenario, and `TestMessyLogs` passing: the six scenarios
  are tidy OpenTelemetry data, the messy-logs scenario is shaped like real production logs (mixed level fields,
  levels only in text, IDs in messages, stack traces).
- **Generic, not one company's logs.** Defaults and heuristics must work for any stack; anything specific to one
  organisation belongs in its config file, not in code or docs.
- **Query changes need a re-record.** If you change a query, the golden tests will report that the call
  was not recorded. Run `make record` to capture fresh fixtures.

- **Docs change with behaviour.** A new config key, tool input or output field updates `docs/` and
  `examples/spoor.yaml` in the same PR.

## Good first issues

- New backends (Datadog, Elasticsearch/OpenSearch): implement `LogStore`/`TraceStore` in `internal/digest` and a
  client in `internal/backend`; see `store_loki.go` and `store_tempo.go` for the pattern
- More log-redaction rules
- Grafana Explore links as an alternative to VMUI
- Scoring for brand-new span names that fail from their first appearance

## Releasing (maintainers)

1. Update `CHANGELOG.md`: move "Unreleased" under the new version.
2. Bump `version` and the image tag in `server.json` to the new version.
3. `git tag vX.Y.Z && git push origin vX.Y.Z`.
4. The `release` workflow runs GoReleaser: binaries for Linux, macOS and Windows (amd64, arm64), checksums, and
   multi-arch images `ghcr.io/asinha07/spoor:X.Y.Z` and `:latest`.
5. Publish to the MCP registry: `mcp-publisher login github && mcp-publisher publish`.

## Commit messages

Use the imperative mood ("Add Loki adapter"). Reference an issue when there is one.

By contributing you agree your work is licensed under Apache-2.0.
