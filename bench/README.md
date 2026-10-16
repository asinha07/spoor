# Benchmark

**Question:** with Spoor available, does an agent reach the correct root cause using fewer tool calls and less
money, at equal or better accuracy?

**Result (Claude Haiku, 60 sessions):** with Spoor the agent was right in **30/30** runs (23/30 without), including
every fault run and every no-fault control, with a median of **10 tool calls instead of 24** and **68% lower
cost**. See [docs/benchmark.md](../docs/benchmark.md).

## Setup

| Arm | Tools the agent gets |
| --- | --- |
| A (baseline) | The official VictoriaMetrics MCP servers for metrics, logs and traces |
| B | The same three servers **plus** Spoor. Spoor only adds a tool, so the comparison is fair. |

- Agent: Claude Code in headless mode (`claude -p`) with `--restricted` (no shell) and `--strict-mcp-config` (only
  the arm's servers).
- Isolation: every session runs with a clean environment and an empty working directory, so it cannot inherit
  another Claude Code session's identity, context or cost tracking, and its file tools see nothing of this repo.
- Servers: all four MCP servers run as long-running HTTP services (`servers.sh`). The official VictoriaMetrics
  server takes about 40 seconds to start, longer than Claude Code waits for a stdio server; without this, arm A
  would silently run without metrics.
- Prompt: one fixed prompt ([`prompts/default.txt`](prompts/default.txt)) that never names the faulty service.
- Scenarios: the six `spoorgen` scenarios (five faults and a no-fault control), ground truth written before any run.
- Runs: 5 per scenario per arm: 6 × 2 × 5 = 60 sessions.

## Measures

| Measure | Source |
| --- | --- |
| Correct root cause | `grade.py` against the ground truth and [`rubric.md`](rubric.md): right service **and** right mechanism |
| Service right | The final line names the right service, whatever the mechanism |
| Tool calls, Used Spoor | Counted from each session's event stream |
| Turns, cost, time | From Claude Code's result record |

## Running it

Requires Docker, Go, `jq`, Python 3 and Claude Code (`claude`) signed in.

```sh
make it-reset && make demo-data && make build   # fresh backends, six scenarios, ground truth in scenarios/out/
./bench/servers.sh up                           # four MCP servers over HTTP on :18081-18084

# 1. Price one run first.
RUNS=1 SCENARIOS=payment-failure ARMS=b MODEL=haiku ./bench/run.sh
jq '{total_cost_usd, num_turns, tool_calls}' bench/results/payment-failure.b.1.json

# 2. Full run. MAX_USD caps each session, TOTAL_USD stops the whole run.
RUNS=5 MODEL=haiku MAX_USD=1 TOTAL_USD=5 ./bench/run.sh

# 3. Grade, then stop the servers.
python3 bench/grade.py bench/results scenarios/out > bench/results/summary.md
./bench/servers.sh down
```

## Replaying your own incidents

`replay.py` runs Spoor (no agent, no model cost) over labelled windows from your own history and grades it:
incidents found, false alarms on quiet windows, latency and backend load. See
[docs/benchmark.md § Replay your own incidents](../docs/benchmark.md#replay-your-own-incidents).

## Results

Latest run (v3, Claude Haiku, 60 sessions, $0.78): with Spoor **30/30** correct (25/25 on faults) against 23/30
without, median **10** tool calls against 24, **68%** lower cost. Analysis and the history of runs:
[docs/benchmark.md](../docs/benchmark.md). Per-run answers: [v3](RESULTS-2026-10-16-haiku-v3.md),
[v2](RESULTS-2026-10-16-haiku-v2.md).

Raw results are written to `bench/results/` (git-ignored). Move or delete it before a new run; `run.sh` skips runs
whose result file already exists.
