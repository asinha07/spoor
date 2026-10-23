# Backends

Spoor reads metrics, logs and traces from separate backends, and each signal picks its own with `type:`. Mix them
as you like: Prometheus metrics with Loki logs and no traces, or New Relic for everything.

| Signal | `type` | Default URL | Notes |
| --- | --- | --- | --- |
| metrics | `victoriametrics` (default) | `http://localhost:8428` | MetricsQL / PromQL |
| metrics | `prometheus` | `http://localhost:9090` | also Mimir, Thanos, Cortex: anything with the Prometheus query API |
| metrics | `newrelic` | NerdGraph | NRQL on Transaction events |
| logs | `victorialogs` (default) | `http://localhost:9428` | LogsQL |
| logs | `loki` | `http://localhost:3100` | LogQL |
| logs | `newrelic` | NerdGraph | NRQL on Log events |
| traces | `victoriatraces` (default) | `http://localhost:10428` | LogsQL |
| traces | `tempo` | `http://localhost:3200` | TraceQL search |
| traces | `newrelic` | NerdGraph | NRQL on Span events |

Choosing a type also sets that backend's defaults (URL, field names, error filter, queries), so a minimal config is
just the types and URLs. Every default can still be overridden; the [configuration reference](configuration.md) lists
all keys. Types can also come from the environment: `SPOOR_METRICS_TYPE`, `SPOOR_LOGS_TYPE`, `SPOOR_TRACES_TYPE`.

Run `spoor doctor` after any change: it checks each backend from Spoor's side and says what it cannot see.

Cause-vs-echo attribution needs traces. Without a traces backend, Spoor still ranks metric and log changes but
cannot say which service a failure started in.

## Prometheus (and Mimir, Thanos)

```yaml
metrics:
  type: prometheus
  url: http://prometheus:9090        # Mimir: http://mimir/prometheus
```

The default queries follow OpenTelemetry naming (`http_server_request_duration_seconds_*{service_name=...}`). If your
metrics use `job` or `http_requests_total`, see the recipes in [configuration](configuration.md#recipes); they are
PromQL and work unchanged on Prometheus. Evidence links open the Prometheus expression browser (`/graph`), or Grafana
Explore when `grafana.metrics_datasource` is set.

## Loki

```yaml
logs:
  type: loki
  url: http://loki:3100
  service_field: service_name        # the stream label holding the service
  severity_fields: [detected_level, level]
  tenant: ""                         # X-Scope-OrgID for multi-tenant Loki
```

- **Service.** `service_field` is a Loki label. Loki 3 sets `service_name` for OpenTelemetry logs; older setups often
  use `app`, `job` or `container`. Dotted names are turned into label names (`service.name` → `service_name`).
- **Errors.** By default an error line is one whose `detected_level` (set by Loki 3) or `level` label or structured
  metadata is an error level. Set `error_filter` to any LogQL pipeline instead, for example
  `|~ "(?i)\\b(error|exception)\\b"` for plain-text logs. `spoor doctor` reports what share of lines has no level.
- **How errors are grouped.** Spoor normalizes each error line into a pattern on the server, with the same steps as
  for VictoriaLogs (JSON message and code fields, first line only, IDs, numbers, UUIDs, IPs): `json`, then
  `label_format` with `regexReplaceAll`, counted by `count_over_time`. Counts are exact.
- **Loki's series limit.** Loki refuses a query that returns more than `max_query_series` series (500 by default).
  Spoor groups all services in one query, then one query per service if Loki refuses. A single service with more
  distinct error patterns than the limit is grouped from its latest 5,000 error lines instead; its counts stay exact,
  and coverage (`logs_sample`) says so. Raising `max_query_series` avoids this.
- **What is not available.** Loki needs a stream selector on every query, so lines with no service label are not
  counted (VictoriaLogs reports them as "belong to no service"). Loki has no UI of its own: set
  `grafana.logs_datasource` for evidence links.

## Tempo

```yaml
traces:
  type: tempo
  url: http://tempo:3200
  service_field: resource.service.name
  error_filter: status = error
  tenant: ""                         # X-Scope-OrgID for multi-tenant Tempo
```

- **Sampled statistics.** Tempo has no aggregation that every deployment enables (TraceQL metrics need the
  metrics-generator's local blocks), so Spoor uses TraceQL search, which every Tempo has. Span error rates and p95s
  come from up to 500 traces per service and range; root-cause attribution reads up to 40 failed and 60 other traces
  whole. Coverage (`traces_sample`) says so on every answer. On the test incidents this was enough to name the right
  origin; on very busy services a rare failure can be missed by the sample.
- **Search range.** Spoor searches with explicit start and end times. Tempo answers those from its storage once
  blocks are flushed (seconds to minutes after ingestion, depending on `max_block_duration`), and from its ingesters
  for the most recent 30 minutes (`query_ingesters_until`). The defaults fit live traffic.
- Tempo has no UI of its own: set `grafana.traces_datasource` for evidence links.

## New Relic

```yaml
metrics: {type: newrelic}
logs:    {type: newrelic}
traces:  {type: newrelic}
newrelic:
  account_id: 1234567                # or NEW_RELIC_ACCOUNT_ID
  region: us                         # or eu
```

Set the key in the environment, not the file: `NEW_RELIC_API_KEY` (or `SPOOR_NEWRELIC_API_KEY`) holding a **User
key** (`NRAK-…`) with read access to the account. Spoor only ever sends the read-only NerdGraph `nrql` query, never
a mutation, and keeps at most five queries in flight, as New Relic recommends.

- **Metrics.** The defaults read APM Transaction events by `appName`: request rate, error rate (`error IS true`)
  and p95 `duration`. They are NRQL templates without `SINCE`/`UNTIL`; Spoor adds the time range. For OpenTelemetry
  services reporting `Metric` data, write your own templates, for example
  `SELECT rate(count(*), 1 second) FROM Span WHERE service.name = {{.Service}} AND span.kind = 'server'`.
  `{{.Service}}` is quoted as an NRQL string.
- **Many services.** With more services than `max_services`, Spoor first scores every service's change in request
  rate, error rate and p95 with two batched queries, and examines the most changed ones. Without this, a capped scan
  takes the first services alphabetically. Coverage (`scope`) says which happened.
- **Logs.** Log events by `entity.name`, errors by `level` or `log.level`. NRQL cannot rewrite text, so Spoor reads
  the 5,000 most frequent distinct error messages (FACET) and groups them into patterns in Go, with the same steps as
  the other backends; counts per service are exact. If New Relic's log patterns are enabled, set
  `logs.pattern_field: newrelic.logPattern` to group by them instead.
- **Spans are a biased sample.** New Relic's agents sample traces and keep slow and failed ones preferentially, so a
  span's error rate or p95 can jump while the service is fine. On New Relic, Spoor reports a span-level change only
  when the service's own latency or error-rate metric moved the same way; the others are counted in coverage
  (`traces_held_back`). Error-origin attribution still uses the failed traces.
- **Cost.** Expect roughly 8–10 NRQL queries per service examined; a scan of 25 services took about 30–40 seconds on a
  real account. NRQL queries count against your account's query limits.

Evidence links open the NRQL query in New Relic's query builder.

## Mixing backends

```yaml
metrics: {type: prometheus, url: http://prometheus:9090}
logs:    {type: loki, url: http://loki:3100}
traces:  {type: tempo, url: http://tempo:3200}
grafana:                              # evidence links through one Grafana
  url: https://grafana.example.com
  metrics_datasource: {uid: prom, type: prometheus}
  logs_datasource: {uid: loki, type: loki}
  traces_datasource: {uid: tempo, type: tempo}
```

Service names must match across backends for findings on the same service to corroborate each other and for
attribution to label echoes. `spoor list-services` shows which signals each service has.

## Testing against real backends

`make it` runs the scenario tests against VictoriaMetrics, VictoriaLogs and VictoriaTraces; `make it-grafana` loads the
payment-failure and no-fault control incidents into real Prometheus, Loki and Tempo and checks the answers. New Relic
cannot run locally: its stores are unit-tested against recorded result shapes, and were checked by hand on a real
account.
