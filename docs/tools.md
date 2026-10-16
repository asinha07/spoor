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
  "summary": "11 change(s) in 3 services. Strongest: payment — Failures start here, in \"POST /charge\" (100% of 100 failed traces). Also: payment — Span \"POST /charge\" errors 0.2% → 43.7% (229 spans). Also: payment — New error log pattern: \"charge failed: card processor returned <N> for order <N>\". Earliest onset 2026-10-16T09:50:00Z. Errors not significantly up (not new) in: product-catalog, recommendation, ad.",
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
  "coverage": {"logs": "ok", "logs_lines": "3349 error lines in 6715 log lines", "metrics": "ok", "traces": "ok"}
}
```

And on the no-fault `control` scenario, where the right answer is "nothing new":

```json
{
  "summary": "No significant change in 7 services during 2026-10-15T11:40:00Z/2026-10-15T12:10:00Z compared with the previous baseline. Errors present before this window and not significantly up, so not a new problem: frontend (33 error lines now vs 28 usual, +18%, all log lines -5%; e.g. \"call to recommendation.List failed with status <N>\"); recommendation (15 error lines now vs 6 usual, +150%, all log lines +11%; e.g. \"recommendation model unavailable\"); product-catalog (14 error lines now vs 11 usual, +27%, all log lines -4%; e.g. \"failed to load product <ID>: database query returned error\").",
  "findings": [],
  "steady_errors": [
    {"service": "frontend", "errors_now": 33, "errors_usual": 28, "change": "+18%", "log_lines_change": "-5%", "example": "call to recommendation.List failed with status <N>"},
    {"service": "recommendation", "errors_now": 15, "errors_usual": 6, "change": "+150%", "log_lines_change": "+11%", "example": "recommendation model unavailable"},
    {"service": "product-catalog", "errors_now": 14, "errors_usual": 11, "change": "+27%", "log_lines_change": "-4%", "example": "failed to load product <ID>: database query returned error"}
  ],
  "truncated": 0,
  "coverage": {"logs": "ok", "logs_lines": "71 error lines in 3364 log lines", "metrics": "ok", "traces": "ok"}
}
```

## `new_errors`

Only the log scorer, for one service or all services at once:

- errors that are **new** (their error family never occurred in the baseline) or **spiking**, each with count,
  first-seen time, one redacted example line and, when the same error appears with different IDs or values, how
  many variants were grouped (`(+12 variants)` in the title);
- services whose **error volume** rose beyond their traffic (`error_volume_up`);
- services whose errors are **steady** (`steady_errors`).

Use it to read the actual error messages behind a `what_changed` finding.

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
| `steady_errors` | array, optional | Up to 3 services whose errors are not significantly above their usual level. Not a cause unless nothing else changed. See below. |
| `truncated` | int | Findings removed to fit `budget_tokens`. Raise the budget or narrow `service` to see them. |
| `coverage` | object | Per signal: `ok`, a no-data note, a no-baseline note, `partial (…)`, `not configured`, `error: …`, or a blind-spot note such as `no error lines: the error filter matched none of …`. Plus notes: `logs_lines` (error, total and unclassified line counts), `logs_patterns`, `logs_volume`, `scope`, `attribution`. See [how-it-works.md § Coverage](how-it-works.md#8-coverage). |

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

### Steady error entry

| Field | Meaning |
| --- | --- |
| `service` | The service. |
| `errors_now` | Error lines in the window. |
| `errors_usual` | Typical (median) error lines per baseline window. |
| `change` | `errors_now` against `errors_usual`, e.g. `+18%`. On small counts a large percentage is noise. |
| `log_lines_change` | Change in all of the service's log lines, when known. Errors rising with traffic read differently from errors rising on flat traffic. |
| `example` | The most frequent error pattern, redacted. |

### Kinds

| Kind | Signal | Meaning |
| --- | --- | --- |
| `error_origin` | traces | Failed traces start in this service |
| `latency_origin` | traces | Added self time is concentrated in this service |
| `error_rate_up` | metrics | Server error ratio rose |
| `latency_up` | metrics | p95 rose |
| `throughput_down` / `throughput_up` | metrics | Request rate fell or rose (including "dropped to zero") |
| `resource_up` | metrics | A configured resource metric (CPU, memory, lag, …) rose |
| `new_log_pattern` | logs | An error whose family never occurred in the baseline |
| `spiking_log_pattern` | logs | A known error far above its usual rate |
| `error_volume_up` | logs | A service's error lines rose clearly above its baseline, and faster than its log lines overall |
| `span_errors_up` | traces | A span name's error rate rose |
| `span_slower` | traces | A span name's p95 rose |

## Server instructions

MCP clients receive these instructions at connection time (they steer how the agent uses the tools):

> Start with what_changed (omit service to scan everything) … Use the default baseline (previous) unless you know the
> backends hold data from a day or a week earlier … steady_errors lists services whose errors are not significantly
> above their usual level … do not report them as the cause unless nothing else changed … A large percentage on
> small counts is noise. … Read coverage: any signal state other than "ok" … means Spoor may be blind there, and an
> empty result is then not evidence of health.

The full text is `Instructions` in [`internal/server/server.go`](../internal/server/server.go).
