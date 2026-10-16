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
