# Benchmark: does Spoor help an agent?

**Question.** Give an AI agent the same incident twice: once with the official VictoriaMetrics, VictoriaLogs and
VictoriaTraces MCP servers, once with those **plus** Spoor. Does it name the right root cause more often, with
fewer tool calls and less money?

**Answer (Claude Haiku, 60 isolated sessions):** yes.

| | Official Victoria MCP servers | + Spoor | Change |
| --- | --- | --- | --- |
| Correct root cause, 5 fault scenarios | 19/25 | **25/25** | +6 |
| Correct answer, all 6 scenarios incl. no-fault control | 23/30 | **30/30** | +7 |
| Right service named | 25/30 | **30/30** | |
| Median tool calls per investigation | 24 | **10** | −58% |
| Median tokens per investigation (incl. cache reads) | 488k | **132k** | −73% |
| Median wall time | 55 s | **23 s** | −58% |
| Total cost, 30 sessions | $0.60 | **$0.19** | −68% |

Arm B called Spoor in every session. Of arm A's seven wrong answers, five blamed the product-catalog database
errors that run in the background all day, and two named the right service but the wrong mechanism.

## Per scenario

| Scenario | A correct | B correct | A median tool calls | B median tool calls | A median cost | B median cost |
| --- | --- | --- | --- | --- | --- | --- |
| payment-failure | 5/5 | 5/5 | 18 | 6 | $0.018 | $0.004 |
| catalog-one-product | 5/5 | 5/5 | 23 | 6 | $0.012 | $0.003 |
| ad-high-cpu | 2/5 | **5/5** | 23 | 12 | $0.020 | $0.007 |
| recommendation-cache-leak | 2/5 | **5/5** | 26 | 14 | $0.023 | $0.008 |
| kafka-queue-problems | 5/5 | 5/5 | 27 | 15 | $0.025 | $0.007 |
| control (no fault) | 4/5 | **5/5** | 22 | 12 | $0.016 | $0.007 |

What this shows:

- **Errors that look obvious are easy either way**, but cost 3–4× less with Spoor (payment, catalog).
- **Resource-driven faults are where raw tools struggle.** CPU saturation and a memory leak produce latency, not
  errors. Without Spoor the agent found the error logs that are always there and blamed them in 4 of 10 runs; with
  Spoor it named the right service and mechanism every time.
- **Saying "nothing is wrong" is the hardest answer.** In the control scenario, Spoor reports no findings and lists
  the background errors under `steady_errors` as "not a new problem". All five runs concluded there was no new
  fault.

Full per-run final answers: [`bench/RESULTS-2026-10-16-haiku-v3.md`](../bench/RESULTS-2026-10-16-haiku-v3.md).

## Method

| | |
| --- | --- |
| Agent | Claude Code, headless (`claude -p`), model Claude Haiku, `--restricted` (no shell, no file edits), `--strict-mcp-config` (only the arm's MCP servers) |
| Arm A | The official VictoriaMetrics, VictoriaLogs and VictoriaTraces MCP servers |
| Arm B | The same three servers **plus** Spoor. Spoor only adds tools; nothing is taken away. |
| Prompt | One fixed prompt for every run ([`bench/prompts/default.txt`](../bench/prompts/default.txt)): users report failing or slow requests in a time window; find where the problem starts, tell cause from affected services, say so if nothing is wrong, and end with a `ROOT CAUSE: <service or "none"> — <description>` line. It never names a service. |
| Scenarios | Six synthetic incidents from `spoorgen` in a 7-service shop: five faults and one no-fault control, each with ground truth written before any run. Background errors run throughout. |
| Runs | 5 per scenario per arm: 60 sessions. |
| Isolation | Each session runs with a clean environment and an empty working directory, so it cannot share identity, context or cost tracking with any other session. All 60 session IDs are unique. |
| Grading | [`bench/grade.py`](../bench/grade.py) checks the final line against the ground truth with the [rubric](../bench/rubric.md): **correct** = right service **and** right mechanism. For the control, correct = "none". |
| Cost, turns, time | From Claude Code's own result record for each session. Tool calls are counted from its event stream. |

Reproduce it, or run it with another model: [`bench/README.md`](../bench/README.md). A full run on Haiku costs
under $1.

## Replay your own incidents

The synthetic benchmark says whether Spoor helps an agent. Whether it works **on your systems** is a different
question, and only your history answers it. [`bench/replay.py`](../bench/replay.py) replays labelled windows
through Spoor with your config and grades the answers:

```sh
cat > windows.csv <<'CSV'
name,since,until,expected_service,expected_kind,notes
payments-outage,2026-06-12T09:40:00Z,2026-06-12T10:10:00Z,payment,,card processor down
slow-search,2026-07-03T18:00:00Z,2026-07-03T18:30:00Z,search,latency_up,index rebuild
quiet-tuesday,2026-06-17T14:00:00Z,2026-06-17T14:30:00Z,none,,normal traffic
traffic-peak,2026-06-20T19:00:00Z,2026-06-20T19:30:00Z,none,,sale, no incident
CSV
python3 bench/replay.py windows.csv --config spoor.yaml --out replay-results
```

It reports, per window and overall: incidents caught as #1 or in the top 3, incidents missed, findings on quiet
windows (false alarms), latency, response size, backend requests and coverage gaps. On the six synthetic scenarios
it gives 5/5 incidents at #1 and no findings on the control window.

Make it a fair test:

- **Label before you replay.** Windows chosen or labelled after looking at Spoor's output measure nothing.
- **Include hard cases:** traffic peaks without incidents, two problems at once, windows where one signal was
  missing, and incidents caused by something outside your telemetry.
- **Mind retention.** The baseline (by default six windows before) must still be in the backends.
- **Keep `windows.csv` and the results private** if they describe your systems; they are not meant for the repo.

## On real production logs

The synthetic benchmark uses tidy data written by Spoor's author. To see what survives contact with real logs,
Spoor was replayed on a production deployment, read-only. The owner's logs, service names and traffic figures stay
private; everything below is an aggregate or a ratio.

**The setup.**
- **System:** about 25 services, logs only (no metrics or traces), read through Grafana's datasource proxy.
- **Log formats:** mixed. About 57% of lines carry no structured level, some services write the level only in the
  message text, some errors are pretty-printed JSON, others have Java log prefixes, and messages embed generated
  IDs.
- **Window:** one fixed 5-minute window, compared with six 5-minute baseline windows; several million log lines in
  the window.
- **Arms:** three, each run three times in rotated order on the same retained data:
  - **direct queries:** four hand-written LogsQL queries returning the full grouped evidence an engineer or agent
    would otherwise read;
  - **Spoor v0.1.1**, the first release;
  - **Spoor now**, after the fixes this replay prompted.

The replays and measurements were run by a separate AI coding agent working only from Spoor's builds, not by the
agent that wrote Spoor.

| | Direct queries | Spoor v0.1.1 | Spoor now |
| --- | --- | --- | --- |
| What you get | Grouped evidence to interpret | Ranked digest | Ranked digest |
| Size of what you read (bytes ÷ 4) | ~296,000 tokens | 735 tokens | **466 tokens** (99.8% smaller than the evidence) |
| Candidates to review in this window | No ranking: 134 exact messages are new against the baseline | 4, all errors that were already there | **0** |
| JSON errors kept distinct | Yes | Yes | **Yes, with their error codes** |
| Baseline counts match the direct queries | (reference) | Not comparable | **170 of 170 service/time rows exact** |
| Services covered | All | 25 of 26 (scan cap) | **26 of 26** |
| Default config on these logs | (n/a) | Reported `ok` while seeing no errors | **Reports the blind spot** |
| Backend queries | 4 | 38 | 6 |
| Median time (3 runs) | 25.8 s | **13.0 s** | 28.2 s |

**What this shows:**
- Spoor turns a 5-minute production window into a digest about 600 times smaller than the raw evidence.
- It cut the candidates to review from four recurring errors to none, while its baseline arithmetic matches
  independent queries exactly.
- On logs it can't fully see, it now says so instead of reporting "no change".

**What it doesn't show:**
- **Speed.** Spoor was about 9% slower than the four direct queries (28.2 s vs 25.8 s), and slower than v0.1.1, which
  was fast because it did less: per-service scans, no family grouping, no baseline verification. Most of the time
  goes to grouping baseline errors when the error filter has to match message text, plus a verification query for
  possible new errors. Making that cheaper is open work.
- **Accuracy, AI cost and time to root cause.** The window has no independently labelled incident, and no agent was
  run, so zero findings is not a correct-root-cause score. The synthetic results above cannot be carried over to
  production; that needs labelled past incidents and quiet windows, with agents run with and without Spoor
  ([`bench/replay.py`](../bench/replay.py) covers the Spoor-only half).
- **Byte-identical diagnostics.** When a service has more than 200 distinct error patterns, patterns tied on count at
  the cutoff can swap between runs, so the number of verified candidates varied (10 to 14) while the findings did
  not.

**What it changed in Spoor.** Each replay found something the synthetic data could not:

| Found on real logs | Change |
| --- | --- |
| Default config read the wrong service and level fields, and reported `ok` | `spoor doctor`; coverage reports zero-match filters, lines without level or service fields |
| IDs split one error into hundreds of patterns (212 candidate findings before normalization) | Built-in ID normalization and error families |
| Every recurring error looked "new" when its exact text was new | "New" means the family never occurred in the baseline, verified past the pattern cap |
| Cutting messages at the first line turned pretty-printed JSON into `{` | Message and code read from JSON fields |
| Baseline volume buckets were shifted a few minutes | Bucket offset fixed; a test checks window totals against a direct count |
| One service fell outside the 25-service scan cap | Logs cover every service |
| A 4-line message in a busy service was flagged as new | A new error needs 10 lines or 5% of its service's errors |
| A query per service per range (38 in all) | Batched queries with exact error counts (6) |

The messy-logs test scenario (`spoorgen --scenario messy-logs`) reproduces each of these shapes with invented data,
so they stay fixed.

## How the benchmark changed Spoor

The benchmark is a development tool as much as a claim. Each run found something:

| Run | A (raw tools) | B (+ Spoor) | What it taught |
| --- | --- | --- | --- |
| v1 | 18/30 | 17/30 | Spoor halved cost but was no more accurate. Every miss came from agents asking for `baseline=1d` when the backends held no data a day earlier: Spoor then reported every recurring error as new. **Fix:** detect empty baselines and say so; require log spikes to pass a Poisson test corrected for the number of patterns checked. |
| v2 | 19/30 | 26/30 | Faults 25/25 with Spoor, but the control was 1/5: agents blamed the background errors even when Spoor reported no change. **Fix:** `steady_errors`, positive evidence that those errors are normal. ([results](../bench/RESULTS-2026-10-16-haiku-v2.md)) |
| v3 | 23/30 | **30/30** | This page. |

Arm A's score moves between runs (18, 19, 23 out of 30) with no change to its tools. That is run-to-run variance
in the model's investigation, and a reminder that five runs per cell is a small sample.

An earlier attempt was discarded entirely: sessions inherited the parent Claude Code session's identity and cost
tracking. The harness now isolates every session.

## Limits, honestly

- **Synthetic incidents, written by Spoor's author.** The scenarios are realistic but not independent. The
  [production replay](#on-real-production-logs) checks the mechanics on real logs, but has no labelled incident. The
  OpenTelemetry Demo check ([deploy/demo](../deploy/demo/README.md)) is an independent sanity check, not yet a
  benchmark.
- **One small model.** Haiku is cheap enough to run many sessions. Larger models may close part of the accuracy gap
  on their own; the tool-call and cost savings are likely to remain, since they come from Spoor doing the
  comparison work in one call.
- **Five runs per cell.** Enough to see large effects (19/25 → 25/25 on faults, 24 → 10 tool calls), not to
  measure small ones.
- **The prompt asks for one root cause.** Incidents with two simultaneous causes are not tested.

Contributions that make the benchmark harder to game are very welcome: independent scenarios, the OpenTelemetry
Demo as a scenario source, other models, other agents.
