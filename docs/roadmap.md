# Roadmap

Spoor's job stays the same: answer "what changed, and where did it start?" for an AI agent, in a few hundred tokens,
read-only and deterministic. This page lists what comes next, roughly in order, and what is deliberately out of scope.
Priorities move with what users report; open an issue to argue for something.

## Shipped

- **0.1:** `what_changed`, `new_errors`, `trace_diff`, `list_services` on the VictoriaMetrics stack; cause vs. echo
  from trace trees; token budgets; the synthetic benchmark.
- **0.2:** built for real logs: error families, JSON message and code fields, mixed level fields, `spoor doctor`,
  redaction of every output field, Grafana-proxy access, steady errors.
- **0.3:** Prometheus (and Mimir, Thanos), Loki, Tempo and New Relic backends, mixable per signal; Grafana-stack
  integration tests; on New Relic, services ranked by change before a capped scan and span changes checked against
  service metrics.

## Next

**Trust on real incidents.** The benchmark is synthetic. Next is replaying labelled past incidents and quiet windows
from real deployments with [`bench/replay.py`](../bench/replay.py), publishing anonymized totals (caught as #1, in
top 3, missed, false alarms), and fixing what that shows. This matters more than any new backend.

**`doctor` for every backend.** On VictoriaLogs, `doctor` suggests the service field, level fields and error filter,
and finds messages split by IDs. Loki, Tempo and New Relic get the same: label and structured-metadata candidates on
Loki, detection of New Relic log patterns (`newrelic.logPattern`) and of OpenTelemetry vs. APM attribute names.

**Faster all-services scans everywhere.** New Relic now ranks all services by change in two batched queries before
the per-service scan. Prometheus and VictoriaMetrics get the same, from a `by (service)` form of the request, error
and latency queries, so a 50-service estate is ranked in three queries instead of capped alphabetically.

**Exact Tempo statistics.** When Tempo's TraceQL metrics are enabled (metrics-generator local blocks), use them for
span error rates and p95s and keep search sampling as the fallback; say which in coverage.

**"Since the last deploy."** Windows that start at the last rollout of a service, from Kubernetes rollout history,
New Relic deployment markers or Grafana annotations, so an agent can ask "what changed since we shipped?"

## Later

- **More backends:** Datadog (metrics, logs, APM), Elasticsearch / OpenSearch logs, Jaeger traces, ClickHouse-based
  stores (SigNoz, HyperDX).
- **More signals:** Kafka consumer-group lag and rebalances, database slow-query digests, Kubernetes events
  (restarts, OOM kills, evictions) as findings that corroborate the rest.
- **Agent frameworks:** a HolmesGPT toolset and drop-in tools for other agent frameworks beyond MCP.
- **Benchmark:** the OpenTelemetry Demo with its built-in faults, larger models, and incident sets written by
  someone other than Spoor's author.

## Out of scope

- **Writing anything.** Spoor never changes your systems, silences alerts or opens tickets. It only reads.
- **An LLM inside.** Same input, same output; the agent does the reasoning, Spoor hands it the evidence.
- **Storing telemetry or alerting.** Spoor queries your backends when asked and keeps nothing.
