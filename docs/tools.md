# Tool reference

Spoor exposes four MCP tools. The CLI has one command per tool with the same inputs as flags
(`spoor what-changed --service payment --since 30m`), and prints the same JSON.

All tools are read-only and idempotent.

## Common inputs

| Input | CLI flag | Default | Meaning |
| --- | --- | --- | --- |
| `service` | `--service` | all services | One service name. Omit to scan every service seen in the window (up to `max_services`). |
| `since` | `--since` | `30m` | Window start: RFC 3339 (`2026-10-16T09:50:00Z`) or a duration ago (`30m`, `2h`, `1d12h`). |
| `until` | `--until` | now | Window end, same formats. |
| `baseline` | `--baseline` | `previous` | `previous`, `1d` or `7d`. See [how-it-works.md](how-it-works.md#1-window-and-baseline). |
| `budget_tokens` | `--budget` | 800 | Response size cap, 200–4000. |

CLI-only: `--config FILE` (or `SPOOR_CONFIG`), `--pretty` (indented JSON).

## `what_changed`

Ranked changes across metrics, logs and traces, with cause/echo attribution. **Start here.**

```sh
spoor what-changed --since 2026-10-16T09:50:00Z --until 2026-10-16T10:20:00Z --budget 600
```

Real output on the `payment-failure` scenario (evidence links removed for brevity):

```json
{
  "summary": "11 change(s) in 3 services. Strongest: payment — Failures start here, in \"POST /charge\" (100% of 100 failed traces). Also: payment — Span \"POST /charge\" errors 0.2% → 43.7% (229 spans). Also: payment — New error log pattern: \"charge failed: card processor returned <N> for order <N>\". Earliest onset 2026-10-16T09:50:00Z. Errors at their usual rate (not new) in: product-catalog, recommendation, ad.",
  "window": "2026-10-16T09:50:00Z/2026-10-16T10:20:00Z",
  "baseline": "2026-10-16T06:50:00Z/2026-10-16T09:50:00Z (previous, 6 windows)",
  "findings": [
    {"service": "payment", "signal": "traces", "kind": "error_origin", "score": 0.99,
     "title": "Failures start here, in \"POST /charge\" (100% of 100 failed traces)"},
    {"service": "payment", "signal": "traces", "kind": "span_errors_up", "score": 0.99,
     "title": "Span \"POST /charge\" errors 0.2% → 43.7% (229 spans)",
     "before": "0.2%", "after": "43.7%", "onset": "2026-10-16T09:50:09Z", "trace_id": "fe4ec592312efb94900bfebf608f2c2e"},
    {"service": "payment", "signal": "logs", "kind": "new_log_pattern", "score": 0.94,
     "title": "New error log pattern: \"charge failed: card processor returned <N> for order <N>\"",
     "before": "0", "after": "1199 in window", "onset": "2026-10-16T09:50:00Z"},
    {"service": "payment", "signal": "metrics", "kind": "error_rate_up", "score": 0.92,
     "title": "Error rate up: 0.2% → 40.5%", "before": "0.2%", "after": "40.5%", "onset": "2026-10-16T09:50:00Z"},
    {"service": "checkout", "signal": "logs", "kind": "spiking_log_pattern", "score": 0.74,
     "title": "Error log pattern spiking: \"call to payment.Charge failed with status <N>\" — originates in payment",
     "before": "4.67 per window", "after": "1199 in window", "onset": "2026-10-16T09:50:00Z"}
  ],
  "truncated": 4,
  "coverage": {"logs": "ok", "metrics": "ok", "traces": "ok"}
}
```

And on the no-fault `control` scenario, where the right answer is "nothing new":

```json
{
  "summary": "No significant change in 7 services during 2026-10-15T08:50:00Z/2026-10-15T09:20:00Z compared with the previous baseline. Errors at their usual rate, present before this window and not a new problem: frontend (30 error lines now vs 36 per window before, …); product-catalog (14 error lines now vs 11.8 per window before, …); ad (5 error lines now vs 7.5 per window before, …).",
  "findings": [],
  "steady_errors": [
    {"service": "frontend", "errors_now": 30, "errors_usual": 36, "example": "call to catalog.GetProduct failed with status <N>"},
    {"service": "product-catalog", "errors_now": 14, "errors_usual": 11.8, "example": "failed to load product <N>: database query returned error"},
    {"service": "ad", "errors_now": 5, "errors_usual": 7.5, "example": "ad index refresh failed"}
  ],
  "truncated": 0,
  "coverage": {"logs": "ok", "metrics": "ok", "traces": "ok"}
}
```

## `new_errors`

Only the log scorer: error patterns that are new or spiking, each with count, first-seen time and one redacted
example line. Also returns `steady_errors`. Use it to read the actual error messages behind a `what_changed` finding.

```sh
spoor new-errors --service payment --since 30m
```

## `trace_diff`

Only the span scorer (plus attribution when no `operation` is given): span names that got slower or started failing,
with p95 and error rate before/after, share of added latency and an example trace ID.

| Extra input | CLI flag | Meaning |
| --- | --- | --- |
| `operation` | `--operation` | Limit to one span name, e.g. `POST /charge`. Attribution is skipped. |

```sh
spoor trace-diff --service payment --since 30m --budget 300
```

## `list_services`

Services seen in the window and which signals have data for each. Takes `since` and `until` only. Use it to find
service names, and to check that all three signals arrive before trusting an empty answer.

```json
{"window": "…", "services": [{"name": "ad", "signals": ["metrics", "logs", "traces"]}, …], "coverage": {…}}
```

## Response envelope

`what_changed`, `new_errors` and `trace_diff` all return this shape:

| Field | Type | Meaning |
| --- | --- | --- |
| `summary` | string | One paragraph for the agent to read first. Mentions steady errors and missing data. |
| `window` | string | The window analysed, `start/end` in RFC 3339 UTC. |
| `baseline` | string | The baseline range, mode and number of sub-windows. |
| `findings` | array | Ranked, highest score first. Empty when nothing changed. |
| `steady_errors` | array, optional | Up to 3 services whose errors run at their usual rate. Not a cause unless nothing else changed. |
| `truncated` | int | Findings removed to fit `budget_tokens`. Raise the budget or narrow `service` to see them. |
| `coverage` | object | Per signal: `ok`, a no-data note, a no-baseline note, `partial (…)`, `not configured` or `error: …`. Plus `scope` and `attribution` notes. See [how-it-works.md § Coverage](how-it-works.md#8-coverage). |

### Finding

| Field | Meaning |
| --- | --- |
| `service` | Where the change was observed (for origins: where it started). |
| `signal` | `metrics`, `logs` or `traces`. |
| `kind` | See below. |
| `score` | 0–1. 0.5 ≈ a robust z-score of 3. Above 0.9 is a strong, corroborated change. |
| `title` | Human-readable, self-contained description. Echoes end in `— originates in <service>`. |
| `before`, `after` | Baseline and window values, formatted with units. |
| `onset` | When the change started (RFC 3339), when known. |
| `example` | Redacted example log line (log findings). |
| `trace_id` | An example trace from the window (span findings). |
| `evidence_link` | VMUI link showing the query behind the finding. Dropped first when trimming to budget, so ask with a larger `budget_tokens` (e.g. 4000) to keep them. |

### Kinds

| Kind | Signal | Meaning |
| --- | --- | --- |
| `error_origin` | traces | Failed traces start in this service |
| `latency_origin` | traces | Added self time is concentrated in this service |
| `error_rate_up` | metrics | Server error ratio rose |
| `latency_up` | metrics | p95 rose |
| `throughput_down` / `throughput_up` | metrics | Request rate fell or rose (including "dropped to zero") |
| `resource_up` | metrics | A configured resource metric (CPU, memory, lag, …) rose |
| `new_log_pattern` | logs | An error pattern absent from the baseline |
| `spiking_log_pattern` | logs | A known error pattern far above its usual rate |
| `span_errors_up` | traces | A span name's error rate rose |
| `span_slower` | traces | A span name's p95 rose |

## Server instructions

MCP clients receive these instructions at connection time (they steer how the agent uses the tools):

> Start with what_changed (omit service to scan everything) … Use the default baseline (previous) unless you know the
> backends hold data from a day or a week earlier; if coverage says the baseline has no data, retry with
> baseline=previous. steady_errors lists services whose errors run at their usual rate: … do not report them as the
> cause unless nothing else changed … Findings are evidence, not conclusions: check coverage for signals that did
> not answer.

The full text is `Instructions` in [`internal/server/server.go`](../internal/server/server.go).
