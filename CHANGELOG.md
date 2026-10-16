# Changelog

## Unreleased

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
