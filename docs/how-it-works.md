# How Spoor decides what changed

Everything below is plain arithmetic over backend query results. There is no model inside Spoor, and the same data
always produces the same answer. Every threshold mentioned here is either a configuration key (see
[configuration.md](configuration.md)) or a named constant in `internal/digest`.

## 1. Window and baseline

A call names a **window** (`since` / `until`, default the last 30 minutes) and a **baseline**:

| `baseline` | Compared against | Use when |
| --- | --- | --- |
| `previous` (default) | `baseline_sub_windows` (default 6) windows of the same length immediately before | Almost always. Needs only a few hours of retention. |
| `1d` | The same window one day earlier, plus the windows just before that | Strongly daily-seasonal traffic, and the backends keep more than a day |
| `7d` | The same window one week earlier, plus the windows just before that | Weekly patterns, and the backends keep more than a week |

`1d` and `7d` refuse a window longer than the shift (a 2-day window cannot be compared with "one day earlier").

Using several sub-windows instead of one lets Spoor measure how much each number normally moves, so a service that
is always noisy needs a bigger change to be reported than one that is always flat.

**Empty baselines are detected.** If the baseline period holds no data for a signal (a `1d` baseline on a system
that started an hour ago, short retention), that signal's findings are dropped and `coverage` says
`no data in baseline period`. Without this, every recurring error looks new. This was the largest source of wrong
answers in the first benchmark run.

## 2. Metrics

For each service, Spoor evaluates the request rate, error rate and p95 latency, plus every configured resource
metric (by default CPU cores, resident memory and Kafka consumer lag), once for the window and once for each
baseline sub-window.

A change is scored with a **robust z-score**:

```
z = (current − median(baseline)) / max(1.4826 × MAD(baseline), relative floor × |median|, absolute floor)
```

The median and median absolute deviation (MAD) ignore a single odd baseline window; the floors stop a perfectly flat
baseline from turning a tiny wobble into a huge z. The z is mapped to a score in 0–1 with `z / (z + 3)`, so **z = 3
gives 0.5, the default `report_threshold`**.

Special cases:

- **Quiet services** (request rate below `min_request_rate`, default 0.05 req/s) skip error-rate and latency
  scoring: three errors out of five requests is not a trend.
- **A service that stops reporting** (request rate drops to zero while the baseline was busy) is reported as
  "Request rate dropped to zero" with a fixed score of 0.9. A silent service is one of the most important things to
  notice and the easiest to miss, because there is nothing to query.
- **Onset**: for each finding Spoor runs a finer range query over the window (steps of a tenth of the window, at
  least a minute) and reports when the metric first crossed halfway between its baseline value and its window
  value.

## 3. Error logs

VictoriaLogs collapses error lines into patterns on the server (`collapse_nums prettify`), so
`charge failed for order 81723` and `charge failed for order 99102` count as one pattern
`charge failed for order <N>`. Spoor compares the top 200 patterns per service in the window and the baseline:

- **New pattern**: present in the window (at least `min_pattern_count` times), absent from the baseline.
  Score starts at 0.5 and grows with volume.
- **Spiking pattern**: at least 4× its usual rate, z ≥ 3, **and** a Poisson tail probability, multiplied by the
  number of patterns checked (Bonferroni correction), below 0.001. Checking 50 patterns per service per call means
  some will blip by chance; the correction keeps those out.

Every log finding includes one example line, redacted (emails, JWTs, bearer tokens, password/secret/API-key
values, card-like digit runs, UUIDs and long hex IDs are masked; IP addresses are not), and its first-seen time.

### Steady errors

Most production systems have errors that are always there. Agents love to blame them. When a service's error lines,
**summed across all its patterns**, run at their usual rate (no more than 2× the baseline, or not significantly
more), Spoor lists the service under `steady_errors` with the count now, the usual count and the most frequent
pattern:

```json
"steady_errors": [
  {"service": "product-catalog", "errors_now": 14, "errors_usual": 11.8,
   "example": "failed to load product <N>: database query returned error"}
]
```

The summary says these errors were "present before this window and not a new problem", and the MCP server
instructions tell the agent not to report them as the cause unless nothing else changed. Services that have a
finding are never listed as steady. Patterns are summed because chronic errors often fragment (one pattern per
product ID), and each fragment alone is too small to judge.

## 4. Spans

For each span name in a service (top 200 by count), Spoor compares the window with the baseline:

- **Errors**: if the error rate rose by at least 5 points with at least 3 failed spans, a binomial z-score is
  computed against the baseline rate (with a 0.5% floor) and mapped like the metrics z.
- **Latency**: if p95 grew at least 1.5× and by at least 20 ms, the score is `1 − 0.75 / ratio` (so 1.5× = 0.5,
  3× = 0.75). The title also shows the span's share of all latency added in the service.

Each span finding carries an example `trace_id` from the window.

## 5. Attribution: cause vs. echo

When payment fails, checkout fails, and the frontend fails. All three show error spikes in every signal. Attribution
uses trace structure to say which one started it. It runs when anything changed and makes three span reads, each
capped at 50,000 rows: failed spans in the window, all spans in the window, and all spans in the baseline.

**Errors.** In each failed trace, Spoor walks the whole tree (including spans that were not themselves marked failed,
which happens with proxies and some frameworks) and finds the deepest failed span with no failed descendant: that is
where the failure started. For each service with an error finding:

- If at least 5 failed traces exist and at least half originate in another service, that service's error findings
  are multiplied by 0.7 and get the suffix `— originates in payment`.
- The origin service gets an `error_origin` finding, scored `0.5 + 0.49 × share`, e.g.
  `Failures start here, in "POST /charge" (100% of 100 failed traces)`.

**Latency.** A span's *self time* is its duration minus its direct children's. If one service accounts for at least
half of all added self time (counting only services whose mean self time grew by 10 ms or more), and its top
operation's mean self time grew at least 1.5×, it gets a `latency_origin` finding, provided some latency finding
exists to explain (in that service or as an echo elsewhere) (`Added latency starts here, in "GetProduct": self time 4 ms → 180 ms`), and latency findings in services that
added less than 20% are multiplied by 0.75 and labelled `— added latency originates in product-catalog`.

## 6. Ranking

1. Findings below `report_threshold` are dropped; duplicates are removed.
2. **Corroboration**: a finding gets +0.05 (capped at 0.99) when another signal for the same service changed within
   two minutes of it. An error-rate rise that starts at the same moment as a new log pattern is more believable than
   either alone.
3. Sort by score, then service, then title, so ties are stable.

## 7. Summary and budget

The summary names the number of changes and services, the top three findings, the earliest onset and the steady
errors. When nothing changed it says so, and says why if the reason is missing data or an empty baseline.

The response is then fitted to `budget_tokens` (default 800, range 200–4000), estimated as characters / 4 of the
compact JSON the agent will actually receive:

1. Evidence links are dropped from the lower half of findings, then all evidence links and log examples.
2. Steady errors are dropped (they are context, not findings).
3. Findings are dropped from the bottom; `truncated` says how many.
4. If even that is too big, coverage notes and the summary are shortened.

## 8. Coverage

Every response carries `coverage`, one entry per signal:

| Value | Meaning |
| --- | --- |
| `ok` | The signal answered and had data |
| `no data for service "<name>"` / `no data in window` | The backend answered but holds nothing for this scope |
| `partial (N/M services failed): …` | Some per-service queries failed; findings for those services are missing |
| `not configured` | The backend's URL is empty |
| `no data in baseline period; …` | Nothing to compare with; that signal's findings were dropped |
| `error: …` | The backend failed or timed out; findings from it are missing |
| `scope` | An all-services scan hit `max_services` |
| `attribution` | Attribution failed; findings are not cause/echo labelled |

## Known limitations

- Attribution reads at most 50,000 span rows per read, unsorted. On very busy systems a trace can be cut, which can
  mislabel an origin. It compares self time against one baseline window, while the scorers use several.
- Log patterns are compared on the top 200 per window, so a pattern that was rare in the baseline can look new.
- A brand-new span name that fails from its first appearance is not scored by the span scorer (metrics, logs and
  attribution still catch it).
- All-services scans visit at most `max_services` services, alphabetically.
