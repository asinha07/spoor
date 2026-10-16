# Configuration

Spoor runs with no config file if your data follows the OpenTelemetry conventions and the backends are on
localhost. Otherwise, write a YAML file and pass it with `--config spoor.yaml` (or `SPOOR_CONFIG=spoor.yaml`).

Every key is optional; anything you leave out keeps its default. **Unknown keys are an error**, so a typo fails at
startup instead of silently falling back to a default.

Check a config without starting the server:

```sh
spoor list-services --config spoor.yaml --since 10m --pretty
```

## Full reference

Every key with its default (the same as [`examples/spoor.yaml`](../examples/spoor.yaml)).

```yaml
timeout: 20s                # per backend request
max_services: 25            # cap for all-services scans (alphabetical)
ignore_services: []         # skipped in scans and attribution, still queryable by name

metrics:
  url: http://localhost:8428
  bearer_token: ""
  ui_url: ""                 # base for evidence links; defaults to url
  service_label: service_name   # label whose values are the service names
  request_rate: 'sum(rate(http_server_request_duration_seconds_count{service_name={{.Service}}}[{{.Window}}]))'
  error_rate: '(sum(rate(http_server_request_duration_seconds_count{service_name={{.Service}},http_response_status_code=~"5.."}[{{.Window}}])) or vector(0)) / sum(rate(http_server_request_duration_seconds_count{service_name={{.Service}}}[{{.Window}}]))'
  latency_p95: 'histogram_quantile(0.95, sum(rate(http_server_request_duration_seconds_bucket{service_name={{.Service}}}[{{.Window}}])) by (le))'
  resources:
    - name: cpu
      unit: cores
      query: 'sum(rate(process_cpu_seconds_total{service_name={{.Service}}}[{{.Window}}]))'
    - name: memory
      unit: bytes
      query: 'max(max_over_time(process_resident_memory_bytes{service_name={{.Service}}}[{{.Window}}]))'
    - name: consumer_lag
      unit: messages
      query: 'max(max_over_time(kafka_consumer_lag{service_name={{.Service}}}[{{.Window}}]))'

logs:
  url: http://localhost:9428
  bearer_token: ""
  ui_url: ""
  service_field: service.name
  error_filter: 'severity_text:~"(?i)^(error|fatal|critical)$"'

traces:
  url: http://localhost:10428
  bearer_token: ""
  ui_url: ""
  service_field: resource_attr:service.name
  name_field: name
  duration_field: duration   # nanoseconds
  error_filter: 'status_code:=2'

scoring:
  report_threshold: 0.5      # 0.5 = robust z-score of 3
  baseline_sub_windows: 6    # history used to measure normal variation
  min_request_rate: 0.05     # req/s; quieter services skip error/latency scoring
  min_pattern_count: 3       # hits a log pattern needs in the window
  min_spans: 5               # spans a span name needs in window and baseline
```

### Query templates

`request_rate`, `error_rate`, `latency_p95` and each `resources[].query` are Go templates that must return **one
number**:

- `{{.Service}}` is replaced by the service name, **already quoted** (`"payment"`), so write
  `service_name={{.Service}}`, not `service_name="{{.Service}}"`.
- `{{.Window}}` is the window length as a MetricsQL duration (`30m`).

Rules of thumb:

- Use `sum(...)` or `max(...)` so the query returns a single series.
- In `error_rate`, wrap the numerator in `(... or vector(0))`; otherwise a service with no errors returns nothing
  instead of 0, and an error spike from zero cannot be measured.
- `latency_p95` must be in **seconds**. Divide by 1000 if your histogram is in milliseconds.
- `request_rate` matters most: it detects services that went silent and gates error and latency scoring
  (`min_request_rate`). The service list itself comes from the values of `service_label`.

### Resource units

`unit` controls how values are printed and the noise floor applied:

| Unit | Printed as | Use for |
| --- | --- | --- |
| `cores` | `0.07 cores` | CPU as cores (rate of CPU seconds). Prefer this over utilisation ratios. |
| `bytes` | `512 MiB` | Memory, disk |
| `messages` | `1200 messages` | Queue or consumer lag (any other unit name is printed after the number) |
| `ratio` | `40.5%` | Shares 0–1 (careful: a share of *all host* cores hides one saturated core) |
| `seconds` | `250ms` | Durations |
| empty | plain number | |

### Environment variables

Environment variables override the file.

| Variable | Overrides |
| --- | --- |
| `SPOOR_CONFIG` | Path of the config file (when `--config` is not given) |
| `SPOOR_METRICS_URL`, `SPOOR_LOGS_URL`, `SPOOR_TRACES_URL` | `*.url` |
| `SPOOR_METRICS_TOKEN`, `SPOOR_LOGS_TOKEN`, `SPOOR_TRACES_TOKEN` | `*.bearer_token` (keep tokens out of files) |
| `SPOOR_REPORT_THRESHOLD` | `scoring.report_threshold` |
| `SPOOR_HTTP_TOKEN` | `serve --auth-token`: bearer token clients must send to the HTTP server |
| `SPOOR_LOG_FORMAT`, `SPOOR_LOG_LEVEL` | `serve --log-format` (`text`, `json`), `--log-level` |

**A backend you don't run:** set its URL to an empty string (`traces: {url: ""}`). Coverage then reports it as
`not configured` and the other signals still answer. (A backend that is configured but unreachable is reported as
`error: …`, also without blocking the others.) Without traces, Spoor still ranks metric and log changes but cannot
separate causes from echoes.

## Recipes

### OpenTelemetry SDKs → OTel Collector → Victoria stack

The defaults. Metrics through the collector's Prometheus remote-write or OTLP exporter to VictoriaMetrics, logs and
traces through OTLP/HTTP to VictoriaLogs (`/insert/opentelemetry/v1/logs`) and VictoriaTraces
(`/insert/opentelemetry/v1/traces`). No config file needed if all three are on localhost.

### Services without consistent HTTP metrics: use span metrics

If your services mix HTTP, gRPC and messaging, or don't export request metrics at all, derive them from traces with
the collector's [`spanmetrics` connector](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/connector/spanmetricsconnector).
This is what the verified [OpenTelemetry Demo config](../deploy/demo/spoor.yaml) does:

```yaml
metrics:
  request_rate: 'sum(rate(traces_span_metrics_calls_total{service_name={{.Service}},span_kind=~"SPAN_KIND_(SERVER|CONSUMER)"}[{{.Window}}]))'
  error_rate: '(sum(rate(traces_span_metrics_calls_total{service_name={{.Service}},span_kind=~"SPAN_KIND_(SERVER|CONSUMER)",status_code="STATUS_CODE_ERROR"}[{{.Window}}])) or vector(0)) / sum(rate(traces_span_metrics_calls_total{service_name={{.Service}},span_kind=~"SPAN_KIND_(SERVER|CONSUMER)"}[{{.Window}}]))'
  latency_p95: 'histogram_quantile(0.95, sum(rate(traces_span_metrics_duration_milliseconds_bucket{service_name={{.Service}},span_kind=~"SPAN_KIND_(SERVER|CONSUMER)"}[{{.Window}}])) by (le)) / 1000'
```

### Classic Prometheus instrumentation (`job` label, `http_requests_total`)

```yaml
metrics:
  service_label: job
  request_rate: 'sum(rate(http_requests_total{job={{.Service}}}[{{.Window}}]))'
  error_rate: '(sum(rate(http_requests_total{job={{.Service}},code=~"5.."}[{{.Window}}])) or vector(0)) / sum(rate(http_requests_total{job={{.Service}}}[{{.Window}}]))'
  latency_p95: 'histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{job={{.Service}}}[{{.Window}}])) by (le))'
```

The service names must match across signals for attribution and cross-signal correlation to work. If metrics use
`job="checkout"` but logs use `service.name="checkout-service"`, Spoor treats them as two services.

### Kubernetes CPU and memory from cAdvisor

```yaml
metrics:
  resources:
    - name: cpu
      unit: cores
      query: 'sum(rate(container_cpu_usage_seconds_total{container={{.Service}}}[{{.Window}}]))'
    - name: memory
      unit: bytes
      query: 'max(max_over_time(container_memory_working_set_bytes{container={{.Service}}}[{{.Window}}]))'
```

### Logs that don't use OTel severity

```yaml
logs:
  service_field: app                 # or kubernetes.container_name, _stream field, …
  error_filter: 'level:~"(?i)^(error|err|fatal|panic)$"'
```

`error_filter` is any LogsQL filter. Test it in VMUI first: it should return only error lines.

### VictoriaMetrics / VictoriaLogs clusters

Point `url` at the select component, including the tenant path for metrics:

```yaml
metrics:
  url: http://vmselect.monitoring.svc:8481/select/0/prometheus
  ui_url: https://vmui.example.com/select/0/prometheus   # what people can open
logs:
  url: http://vlselect.monitoring.svc:9471    # your vlselect address
traces:
  url: http://vtselect.monitoring.svc:10471   # your vtselect address
```

Spoor appends the API path (`/api/v1/query`, `/select/logsql/query`) to whatever base URL you give, so path prefixes
from proxies or tenants work.

Spoor queries the default logs tenant (`0:0`); per-tenant `AccountID` / `ProjectID` headers are not supported yet.

### Leave noise out

```yaml
ignore_services: [load-generator, otel-collector, synthetic-checks]
```

Ignored services are skipped in all-services scans and are never named as an error or latency origin. You can still
ask about one by name.

### Tuning sensitivity

| Symptom | Change |
| --- | --- |
| Too many small findings | Raise `report_threshold` (0.6 ≈ z of 4.5) or `min_pattern_count` |
| A real incident was missed | Lower `report_threshold` to 0.4, check `coverage`, check the query returns data |
| Short history (new system, short retention) | Lower `baseline_sub_windows` (minimum 2) |
| Low-traffic services ignored | Lower `min_request_rate` |
| `coverage.scope` says the scan was capped | Raise `max_services`, or ask per service |
