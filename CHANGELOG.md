# Changelog

## Unreleased

### Added
- Website at [telespoor.com](https://telespoor.com): install, connect (Claude Code, Codex, Cursor, Claude Desktop,
  VS Code, shared HTTP), tools and benchmark on one page. Source in `site/`.
- Codex setup in the getting-started guide.

### Changed
- `server.json` points `websiteUrl` at telespoor.com (takes effect in the registry with the next release).

## 0.2.1 (2026-10-16)

### Changed
- The container image starts MCP over stdio by default (`CMD ["serve"]`), the convention for MCP images, so MCP
  directories and `docker run -i ghcr.io/asinha07/spoor` work as is. For a shared HTTP server pass
  `serve --http :8080`; the Kubernetes, Compose and systemd files already do.

## 0.2.0 (2026-10-16)

Fixes from a trial on real production logs (mixed formats, IDs in messages, Grafana-only access).

### Added
- `spoor doctor`: checks what Spoor can see in each backend (service and level fields, lines without them, levels
  only in message text, error filter matches, messages fragmented by IDs, metric queries that return nothing) and
  suggests config.
- `logs.severity_fields`: the error filter is built from your level fields, and coverage reports the share of lines
  with none of them. `logs.error_filter` now overrides it (default changed from a fixed `severity_text` filter to
  `severity_fields: [severity_text]`; behaviour is the same for OpenTelemetry logs).
- Error-message normalization before grouping: first line only, leading timestamps dropped, letter-digit IDs
  (`normalize_ids`), user rules (`logs.normalize`), or grouping by a field (`logs.pattern_field`).
- Error families: messages that differ only by IDs, quoted values or `key=value` values are one error. "New" now
  means the family never occurred in the baseline; findings show how many variants were grouped.
- `error_volume_up` finding: a service's error lines rose beyond its baseline and faster than its log lines.
- `steady_errors` entries show `change` and `log_lines_change`.
- Coverage notes `logs_lines`, `logs_patterns`, `logs_volume`; blind spots (error filter matching nothing, lines
  without a service field) replace `ok` and are named in the summary.
- Redaction of every output field (summary, titles, before/after, coverage), IPv4 addresses and letter-digit IDs;
  user `redact` rules.
- HTTP Basic auth per backend (`username`, `password`, `SPOOR_*_USER`, `SPOOR_*_PASSWORD`), for Grafana's datasource
  proxy; `grafana` block for Grafana Explore evidence links.
- `--stats` on CLI commands: backend request count and time.
- `bench/replay.py`: replay labelled windows from your own history and grade Spoor.
- `messy-logs` test scenario (`spoorgen --scenario messy-logs`, `deploy/test/messy.yaml`) and integration test.

### Docs
- Benchmark: production replay on real logs (anonymized aggregates): output size, candidates, count agreement,
  coverage and timing against direct queries and v0.1.1.

### Fixed
- Sorts that feed a limit (log patterns, span names, doctor values) break ties by name, so repeated runs keep
  the same patterns and report the same diagnostics.
- Line-total buckets were offset from the baseline sub-windows (VictoriaLogs applies a bucket offset backwards),
  which skewed error share and `log_lines_change`. An integration test now checks the window total against a direct
  count.

### Changed
- Logs are scored for all services in three or four parallel queries instead of two per service; error counts
  per sub-window come from the pattern queries (`total_stats`), so a text-matching error filter runs once per range.
- Logs cover every service; `max_services` now caps only metrics and traces.
- JSON error messages: text and code are taken from `logs.message_fields` / `logs.code_fields` (new) instead of
  cutting at the first line, which turned pretty-printed JSON into `{`.
- Error families ignore log-prefix brackets (`[ERROR] [thread] [correlationId:…]`) but keep class names.
- A new error needs 10 lines, or 5% of its service's error lines, to be reported.
- When logs are blind, the summary leads with it instead of "No significant change".
- `logs_patterns` says how many possible new errors were actually verified.
- `spoor doctor` ignores numeric level fields (log4j `level_value`), flags JSON errors without a message field, and
  words variant counts as grouped rather than unnormalized.
- New errors are verified against the full baseline when a service has more than 200 distinct error patterns.
- A partial metric failure is reported as `partial (…)` instead of being hidden.
- Self time subtracts the union of child spans (parallel calls counted once) when span start times are available
  (`traces.start_field`).
- With findings present, the summary lists steady-error services by name only.


## 0.1.1 (2026-10-16)

First release with published binaries and images. (`v0.1.0` was tagged before the release workflow was in
the repository, so it has no artifacts.) Adds `server.json` and an image label for the official MCP registry.

## 0.1.0

### Added
- MCP server with four read-only tools: `what_changed`, `new_errors`, `trace_diff`, `list_services`.
- Metrics scorer (request rate, error rate, p95, CPU, memory, consumer lag) against six baseline windows.
- Error-log pattern scorer using VictoriaLogs `collapse_nums`, with redacted examples.
- Span scorer (error rate, p95, share of added latency, example trace IDs).
- Root-cause attribution from trace structure: error origins and self-time growth; echoes in callers are demoted.
- Token budgeting on every tool.
- `spoor` CLI with stdio and streamable-HTTP transports.
- `spoorgen` synthetic incident generator with six scenarios and ground truth.
- Unit, golden (recorded fixtures), MCP protocol, read-only, integration and scenario tests.
- Coverage reports errors and missing data per signal; a service whose traffic stops is flagged.
- Responses are compact JSON without HTML escaping, so log patterns cost fewer tokens.
- Empty baselines are reported instead of producing findings; tool instructions recommend `baseline=previous`.
- Error origins are found through spans that were not marked failed (seen on the OpenTelemetry Demo).
- Log spikes must pass a Poisson tail test corrected for the number of patterns checked.
- `ignore_services` setting for load generators, collectors and browser clients.
- Verified OpenTelemetry Demo v3.1 configuration (`deploy/demo/`).
- `steady_errors`: services whose errors run at their usual rate are listed as "not a new problem", so agents stop
  blaming background errors.
- HTTP server: bearer-token auth (`--auth-token`, `SPOOR_HTTP_TOKEN`), `/healthz`, Prometheus `/metrics`
  (tool calls, backend requests, durations, findings), structured tool-call logs (`--log-format json`,
  `--log-level`; debug logs every backend request).
- Strict config: unknown keys are rejected at startup.
- Deployment files: Kubernetes (Kustomize), Docker Compose, hardened systemd unit.
- Documentation: getting started, production, configuration, tools, architecture, how it works, benchmark,
  comparison, troubleshooting, FAQ (`docs/`).
- Agent benchmark v3 (Haiku, 60 sessions): 30/30 correct with Spoor vs 23/30 without, 10 vs 24 median tool calls,
  68% lower cost.
