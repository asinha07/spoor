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

Logs are scored for **all services at once** (they are not limited by `max_services`), in three or four queries
whatever the number of services, run in parallel:

| Query | Purpose |
| --- | --- |
| Error patterns in the window | Top 200 patterns per service (`sort … partition by`), plus each service's exact error-line total (`total_stats`, computed before the limit) |
| Error patterns in the baseline | Same, plus each service's exact error lines per baseline sub-window |
| Line totals | All lines and lines with none of the `severity_fields`, per service, for the window and each sub-window, plus lines with no service field. No error filter, so it stays cheap even when the error filter matches message text. With `baseline=1d`/`7d` this is two queries. |
| Verification (only when needed) | A direct baseline count for "new" errors, when a service's baseline hit the 200-pattern cap |

The error filter, the expensive part when it matches message text, runs once over the window and once over the
baseline.

### From lines to errors

Real error messages carry IDs, values and stack traces, so one error produces thousands of distinct strings. Spoor
turns each message into a **pattern** on the server, without changing stored logs:

1. **JSON messages:** if the message is JSON, the first of `logs.message_fields` found (`message`, `msg`,
   `error.message`, …) becomes the text, and the first of `logs.code_fields` found (`error_code`, `code`, …) is put
   in front of it after the steps below, so a code like `TEMPLATE_404` is kept as is. Pretty-printed JSON errors keep
   their identity instead of all becoming `{`;
2. **plain text:** keep the first line only (stack traces vary). JSON (starting with `{` or a JSON array) is never
   cut; a line starting with `[ERROR]` or similar is;
3. collapse whitespace and drop a leading timestamp;
4. apply your `logs.normalize` rules;
5. replace tokens that mix letters and digits with `<ID>` (`normalize_ids`: `ord_7f3k2m9q`, `OLJCESPC7Z`, `ord_99a1`);
6. collapse numbers, UUIDs and IPs (`collapse_nums`).

Patterns are then grouped into **families**: the pattern's first eight fixed words, ignoring quoted values,
`key=value` values, anything containing a digit, a leading level word, and log-prefix brackets that hold a level or a
variable value (`[ERROR]`, `[http-nio-8080-exec-1]`, `[correlationId:…]`). A bracketed class name is kept without
its line number (`[InvoiceController.java:88]` counts as `invoicecontroller.java`), so two different
errors from one class stay apart. `no route matched for "home"` and
`no route matched for "pricing-x2"` are one family. Findings name the most frequent pattern of a family and
say how many variants were grouped. With `logs.pattern_field` set, the field's value is the pattern instead.

### New and spiking errors

Families are compared between window and baseline:

- **New error** (`new_log_pattern`): the family never occurred in the baseline, and it has at least 10 lines in the
  window, or at least `min_pattern_count` lines making up 5% or more of the service's error lines. Four lines of a
  new message in a service that logs hundreds of errors per window are noise. If a service's baseline patterns hit the 200 cap, a family missing from that list may still have
  occurred below it, so Spoor checks with a direct count (its distinctive words, case-insensitive) before calling it
  new. A new variant of an existing error is not new. Score starts at 0.5 and grows with volume.
- **Spiking error** (`spiking_log_pattern`): at least 4× its usual rate, z ≥ 3, **and** a Poisson tail probability,
  multiplied by the number of families checked (Bonferroni correction), below 0.001. Checking dozens of families
  per call means some will blip by chance; the correction keeps those out.

### Error volume

A service can get worse without any new error: the same errors, more often. Spoor compares each service's error
lines in the window with its baseline sub-windows using the same robust z-score as metrics (15% relative noise
floor, 5-line absolute floor). When total line counts are known, the **share** of error lines must rise as well, so
a traffic increase alone (errors and lines both +60%) is not reported. A clear rise (z ≥ 3 on both, at least +30%,
and a small Poisson tail probability) becomes an `error_volume_up` finding:

```
Error log volume up +70%: 174.5 → 297 lines per window (share of log lines 4.78% → 8.20%)
```

### Steady errors

Most production systems have errors that are always there, and agents love to blame them. Services with error lines
but no log finding are listed under `steady_errors`, with the change against the baseline and, when known, the change
in all their log lines:

```json
"steady_errors": [
  {"service": "product-catalog", "errors_now": 14, "errors_usual": 11, "change": "+27%",
   "log_lines_change": "-4%", "example": "failed to load product <ID>: database query returned error"}
]
```

Steady means *not significantly up*, not *healthy*. The summary says these errors were "present before this window"
and the MCP instructions tell the agent not to report them as the cause unless nothing else changed. A large
percentage on a handful of lines is noise; the statistics account for that, the percentage does not.

Every log finding carries one example line and its first-seen time. All text is redacted (see
[§ 9](#9-redaction)).

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

**Latency.** A span's *self time* is its duration minus the time covered by its direct children. With span start
times (`traces.start_field`, default `start_time_unix_nano`), children that run in parallel are counted once: Spoor
takes the union of their intervals, clipped to the parent. Without start times, child durations are summed. If one service accounts for at least
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

Every response carries `coverage`. One entry per signal:

| Value | Meaning |
| --- | --- |
| `ok` | The signal answered and had data |
| `no data for service "<name>"` / `no data in window` | The backend answered but holds nothing for this scope |
| `no data in baseline period; …` | Nothing to compare with; that signal's findings were dropped |
| `no error lines: the error filter matched none of N log lines; …` | Logs arrive, but Spoor cannot see errors in them. Not evidence of health. |
| `N log lines in window, but none has a <field> field; …` | Logs arrive, but the service field is wrong |
| `partial (…)` | Some queries failed; the findings shown are incomplete |
| `not configured` | The backend's URL is empty |
| `error: …` | The backend failed or timed out; findings from it are missing |

And notes:

| Key | Meaning |
| --- | --- |
| `logs_lines` | Error lines and total lines in scope, the share with none of the `severity_fields`, and lines with no service field |
| `logs_patterns` | A service had more than 200 distinct error patterns; says how many possible new errors were checked against the full baseline (or that none needed checking) |
| `logs_volume` | Line totals failed; error counts still work, but error share and blind-spot checks are skipped |
| `scope` | An all-services scan hit `max_services` |
| `attribution` | Attribution failed; findings are not cause/echo labelled |

When 30% or more of log lines carry no severity field, or no service field, the summary says so too.
`spoor doctor` turns these into concrete config suggestions.

## 9. Redaction

Every text field Spoor returns (summary, titles, examples, before/after values, steady-error examples, coverage
notes) is redacted at the end of each call, not just log examples:

- emails, JWTs, bearer/basic tokens, `password=`/`secret=`/`token=`/API-key values;
- card-like digit runs, IPv4 addresses, UUIDs, hex strings of 24+ characters;
- identifiers: tokens of six or more characters with a segment that mixes digits and at least two letters
  (`ord_7f3k2m9q`, `OLJCESPC7Z`). Service names, timestamps and numbers with units (`1500ms`) are kept;
- your own `redact` rules.

Trace IDs and evidence links are kept: they are needed to verify a finding. Identifiers made only of letters cannot
be told apart from words; add `redact` rules for them.

## Known limitations

- Attribution reads at most 50,000 span rows per read, unsorted. On very busy systems a trace can be cut, which can
  mislabel an origin. It compares self time against one baseline window, while the scorers use several.
- JSON errors whose text is in a key not listed in `logs.message_fields` keep their whole body as the pattern:
  still distinct, but titles are noisy. `spoor doctor` reports them.
- Families are a heuristic (first eight fixed words). Two different errors that share a long prefix and differ
  only in a quoted value or a token with digits are merged; set `logs.pattern_field` if you have error codes.
- Metrics and traces are still queried per service (a full scan of 25 services with all signals is around 100
  small queries); logs are batched. Pass `service` when you know it.
- A brand-new span name that fails from its first appearance is not scored by the span scorer (metrics, logs and
  attribution still catch it).
- All-services scans visit at most `max_services` services, alphabetically.
