# Configuration

Spoor runs with no config file if your data follows the OpenTelemetry conventions and the backends are on
localhost. Otherwise, write a YAML file and pass it with `--config spoor.yaml` (or `SPOOR_CONFIG=spoor.yaml`).

Every key is optional; anything you leave out keeps its default. **Unknown keys are an error**, so a typo fails at
startup instead of silently falling back to a default.

**Start with `spoor doctor`.** It reads a sample of your backends and tells you what Spoor can and cannot see:
which field holds the service name, which fields hold the log level, how many lines carry neither, whether the
error filter matches anything, and which error messages are split into many patterns by IDs. It ends with a
suggested config:

```sh
spoor doctor --since 15m                      # with the defaults
spoor doctor --since 15m --config spoor.yaml  # check your config
```

Then check the services and signals Spoor finds:

```sh
spoor list-services --config spoor.yaml --since 10m --pretty
```

Prometheus, Loki, Tempo or New Relic: set each signal's `type` first; it switches the defaults below to that
backend's. Setup per backend: [backends](backends.md).

## Full reference

Every key with its default (the same as [`examples/spoor.yaml`](../examples/spoor.yaml)).

```yaml
# Spoor configuration reference. Every key is optional; these are the defaults.
# Unknown keys are rejected. Run `spoor doctor --config this-file` to check it
# against your backends; it suggests service and severity fields for your logs.
# Query templates receive {{.Service}} (already quoted) and {{.Window}} (e.g. 30m).
#
# These are the defaults for the VictoriaMetrics stack. Each signal's `type`
# (victoriametrics/prometheus/newrelic, victorialogs/loki/newrelic,
# victoriatraces/tempo/newrelic) switches its defaults to fit that backend;
# see docs/backends.md.

timeout: 20s
max_services: 25            # cap for all-services scans (metrics and traces are queried per service)
ignore_services: []         # e.g. [load-generator, otel-collector]: skipped in scans and attribution

# Extra redaction, applied to every text field Spoor returns, after the
# built-in rules (emails, tokens, JWTs, card-like numbers, IPs, UUIDs, long hex,
# and identifiers that mix letters and digits).
redact: []
#  - regex: 'sess_[a-z]{12}'
#    replace: '<SESSION>'

# New Relic account for signals of type newrelic. Keep the User API key
# (NRAK-...) in NEW_RELIC_API_KEY or SPOOR_NEWRELIC_API_KEY, not here.
newrelic:
  account_id: 0              # or NEW_RELIC_ACCOUNT_ID
  api_key: ""
  region: us                 # us or eu
  url: ""                    # NerdGraph endpoint override (proxies, tests)

# Evidence links open Grafana Explore instead of VMUI when a datasource is set.
grafana:
  url: ""                    # e.g. https://grafana.example.com
  metrics_datasource: {uid: "", type: ""}   # type e.g. prometheus
  logs_datasource: {uid: "", type: ""}      # type e.g. victoriametrics-logs-datasource
  traces_datasource: {uid: "", type: ""}

metrics:
  type: victoriametrics      # or prometheus (also Mimir, Thanos), newrelic; or SPOOR_METRICS_TYPE
  url: http://localhost:8428
  bearer_token: ""           # or SPOOR_METRICS_TOKEN
  username: ""               # HTTP Basic auth, used when no bearer token is set
  password: ""               # or SPOOR_METRICS_USER / SPOOR_METRICS_PASSWORD
  ui_url: ""                 # base for evidence links; defaults to url
  service_label: service_name
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
  type: victorialogs         # or loki, newrelic; or SPOOR_LOGS_TYPE
  url: http://localhost:9428
  tenant: ""                 # Loki: X-Scope-OrgID
  bearer_token: ""
  username: ""
  password: ""
  ui_url: ""
  service_field: service.name   # a label for Loki, an attribute for New Relic
  # Fields that may hold a log level. Spoor builds the error filter from them
  # (values err, error, fatal, crit, critical, alert, emerg, panic, severe) and
  # reports the share of lines that carry none of them.
  severity_fields: [severity_text]
  # Replaces the filter built from severity_fields, in the backend's language
  # (LogsQL, a LogQL pipeline, or an NRQL condition). Use it when some levels
  # are only in the message text; `spoor doctor` suggests one for LogsQL.
  error_filter: ""
  # Replace tokens that mix letters and digits (order, cart, product IDs) with
  # <ID> before grouping error lines, so one error is one pattern.
  normalize_ids: true
  # Extra replacements applied to messages before grouping (RE2 syntax).
  normalize: []
  #  - regex: '\b(ord|sku)_[A-Za-z0-9]+\b'
  #    replace: '${1}_<ID>'
  # Group error lines by this field (an error code, exception class) instead
  # of by message text.
  pattern_field: ""
  # JSON messages: the first of these keys found is the error text, and the
  # first code key found is put in front of it. [] turns it off.
  message_fields: [message, msg, error.message, error_message, errorMessage, exception.message, error, err, exception]
  code_fields: [error_code, errorCode, error.code, code, exception.type, error.type]

traces:
  type: victoriatraces       # or tempo, newrelic; or SPOOR_TRACES_TYPE
  url: http://localhost:10428
  tenant: ""                 # Tempo: X-Scope-OrgID
  bearer_token: ""
  username: ""
  password: ""
  ui_url: ""
  service_field: resource_attr:service.name
  name_field: name
  duration_field: duration   # nanoseconds
  start_field: start_time_unix_nano  # lets self time count parallel child calls once; "" to sum children
  error_filter: 'status_code:=2'   # Tempo: status = error; New Relic: error IS true

scoring:
  report_threshold: 0.5      # 0.5 = robust z-score of 3
  baseline_sub_windows: 6    # history used to measure normal variation
  min_request_rate: 0.05     # req/s; quieter services skip error/latency scoring
  min_pattern_count: 3       # error lines an error needs in the window to count
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
| `SPOOR_METRICS_USER` / `_PASSWORD`, `SPOOR_LOGS_USER` / `_PASSWORD`, `SPOOR_TRACES_USER` / `_PASSWORD` | `*.username`, `*.password` (HTTP Basic auth; ignored when a bearer token is set) |
| `SPOOR_METRICS_TYPE`, `SPOOR_LOGS_TYPE`, `SPOOR_TRACES_TYPE` | `*.type` (and that backend's defaults) |
| `NEW_RELIC_API_KEY` or `SPOOR_NEWRELIC_API_KEY` | `newrelic.api_key` (a User key, `NRAK-…`) |
| `NEW_RELIC_ACCOUNT_ID` or `SPOOR_NEWRELIC_ACCOUNT_ID`, `SPOOR_NEWRELIC_REGION` | `newrelic.account_id`, `newrelic.region` |
| `SPOOR_REPORT_THRESHOLD` | `scoring.report_threshold` |
| `SPOOR_HTTP_TOKEN` | `serve --auth-token`: bearer token clients must send to the HTTP server |
| `SPOOR_LOG_FORMAT`, `SPOOR_LOG_LEVEL` | `serve --log-format` (`text`, `json`), `--log-level`; `--log-level debug` on any command logs every backend query |

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

### Logs that aren't OpenTelemetry

Real logs rarely look like the defaults: the service is in a `container` or `app` field, teams use different
level fields, and some services write their level only inside the message text. Run `spoor doctor` first; a
typical result looks like this:

```yaml
logs:
  service_field: container                 # whichever field doctor finds on ~100% of lines
  severity_fields: [level, log.level]      # every field that holds a level
  # Optional: also catch lines whose level is only in the text, e.g.
  # "2026-10-16T12:00:00Z [ERROR] payment declined". doctor writes this for you
  # when it finds such lines.
  error_filter: '(level:~"(?i)^(err|error|fatal|crit|critical|alert|emerg|emergency|panic|severe)$" OR log.level:~"(?i)^(err|error|fatal|crit|critical|alert|emerg|emergency|panic|severe)$" OR (NOT level:* NOT log.level:* _msg:~"^(\\S+\\s+){1,3}\\[?(ERROR|FATAL|CRITICAL|SEVERE|PANIC)\\]?[\\s:]"))'
```

- `severity_fields` alone is enough when every service has a structured level. Spoor builds the error filter and
  reports, in `coverage.logs_lines`, the share of lines that carry none of the fields.
- `error_filter` is any LogsQL filter and replaces the built one. Test it in VMUI first: it should return only
  error lines. Regex matching on message text is slower than field filters on large volumes.
- If the error filter matches nothing, Spoor says so in `coverage.logs` and in the summary rather than reporting
  "no change". The same goes for lines that have no service field.

[`deploy/test/messy.yaml`](../deploy/test/messy.yaml) is a complete example, used by the `messy-logs` test
scenario (`spoorgen --scenario messy-logs`).

### Error messages full of IDs

`cart ord_7f3k2m9q not found` and `cart ord_2p8x4w1z not found` are the same error. Spoor normalizes messages before
grouping them: for JSON messages it takes the error text and code from `logs.message_fields` / `logs.code_fields`,
for plain text it keeps the first line only (stack traces vary), drops a leading timestamp, replaces tokens that mix
letters and digits with `<ID>` (`normalize_ids: true`), and collapses numbers, UUIDs and IPs. It then groups
patterns into error *families* by their fixed words, so a message that differs only in a quoted name or a
`key=value` is still one error.

If `spoor doctor` reports a message with many variants, a value in it still varies. Add a rule:

```yaml
logs:
  normalize:
    - regex: '\b(ord|sku)_[A-Za-z0-9]+\b'   # your ID formats; RE2 syntax
      replace: '${1}_<ID>'
    - regex: 'session [a-z]{12}'
      replace: 'session <ID>'
```

If your logs carry a stable error code or exception class, group by it instead of by text:

```yaml
logs:
  pattern_field: error.code
```

### Behind Grafana

If you reach VictoriaMetrics, VictoriaLogs or VictoriaTraces through Grafana, point Spoor at Grafana's datasource
proxy. Spoor appends the API path, so this works for every backend:

```yaml
logs:
  url: https://grafana.example.com/api/datasources/proxy/uid/<logs-datasource-uid>
metrics:
  url: https://grafana.example.com/api/datasources/proxy/uid/<metrics-datasource-uid>
grafana:
  url: https://grafana.example.com
  logs_datasource: {uid: <logs-datasource-uid>, type: victoriametrics-logs-datasource}
  metrics_datasource: {uid: <metrics-datasource-uid>, type: prometheus}
```

Authenticate with a Grafana service-account token (`SPOOR_LOGS_TOKEN=glsa_…`) or with a user and password
(`SPOOR_LOGS_USER`, `SPOOR_LOGS_PASSWORD`, sent as HTTP Basic auth). Give the account Viewer access only. The
`grafana` block makes evidence links open Grafana Explore with the query filled in.

### Redact more

Spoor redacts every text field it returns: emails, JWTs, bearer tokens, password/secret/API-key values, card-like
numbers, IPv4 addresses, UUIDs, long hex strings, and tokens that mix letters and digits (most generated IDs).
Identifiers made only of letters cannot be told apart from words, so add rules for yours:

```yaml
redact:
  - regex: 'sess_[a-z]{12}'
    replace: '<SESSION>'
```

Redaction is pattern-based. Review `spoor new-errors` output on your data before connecting an external model.

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
| One error shows up as many findings or variants | Add a `logs.normalize` rule or set `logs.pattern_field` (see `spoor doctor`) |
| Error volume findings on services whose traffic grows | Expected only when errors grow faster than log lines; check `log_lines_change` |
