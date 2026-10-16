# How Spoor compares

Spoor is a **tool for agents**, not an agent. It answers one question well, "what changed, and where did it start?",
and leaves reasoning, follow-up queries and the final explanation to whichever agent calls it. That shapes where it
fits next to other options.

## Raw query MCP servers

*Examples: the official VictoriaMetrics, VictoriaLogs and VictoriaTraces MCP servers; Prometheus, Loki and Tempo
MCP servers.*

These give an agent the query languages and the data. The agent has to know what to query, run the baseline
comparison itself, page through results and work out cause versus echo, spending a tool call and tokens on each
step.

Spoor does the comparison, statistics and trace walking in code and returns a few hundred tokens. In the
[benchmark](benchmark.md), adding Spoor next to the three official servers cut the median number of tool calls
from 24 to 10 and the cost by 68%, while raising correct root causes on fault scenarios from 19/25 to 25/25.

**Use both.** Spoor narrows the search; raw query tools let the agent look closer at what Spoor pointed to.

## Vendor investigation features

*Examples: Grafana Cloud's Sift investigations (exposed to agents through the Grafana MCP server, along with tools
such as error-pattern and slow-request finders), Datadog Watchdog, New Relic's AI features.*

These are closest in spirit: automated "what looks unusual" analyses over a vendor's data. They are tied to that
vendor's platform and pricing, and their internals are not open.

Spoor is open source, runs anywhere next to self-hosted VictoriaMetrics, VictoriaLogs and VictoriaTraces, is
deterministic and testable, and its every finding links to the query behind it.

## Investigation agents and AI SRE products

*Examples: HolmesGPT (open source), and commercial AI SRE products such as Resolve.ai, Traversal and Cleric.*

These are agents: they plan an investigation, call many tools (Kubernetes, logs, metrics, runbooks, tickets) and
write a conclusion. Spoor can be one of their tools. An investigation agent that calls `what_changed` first spends
fewer steps finding where to look. A HolmesGPT toolset is on the roadmap.

## Anomaly detection

*Examples: `vmanomaly`, Prometheus recording rules with z-scores, alerting.*

Anomaly detectors run continuously and decide when to page someone. Spoor runs on demand, for a window someone
already cares about, compares it with its own baseline, and explains rather than alerts. They complement each other:
an alert fires, an agent asks Spoor what changed in that window.

## At a glance

| | Spoor | Raw query MCP servers | Vendor investigations | Investigation agents |
| --- | --- | --- | --- | --- |
| What it is | Tool (MCP + CLI) | Tool (MCP) | Platform feature | Agent |
| LLM inside | No | No | Usually | Yes |
| Answer size | Bounded (`budget_tokens`) | Unbounded | Varies | n/a |
| Cross-signal (metrics + logs + traces) | Yes | One signal per server | Yes | Via its tools |
| Cause vs. echo from traces | Yes | No (agent must do it) | Varies | Via reasoning |
| Self-hosted, open source | Yes, Apache-2.0 | Yes | No | Some |
| Backends | VictoriaMetrics stack (Prometheus / Loki / Tempo planned) | Their own | Their own | Many |

## When Spoor is not the right tool

- You don't have traces: Spoor still ranks metric and log changes, but cannot separate causes from echoes.
- You need continuous alerting: use an anomaly detector or alert rules.
- You are not on the VictoriaMetrics stack yet: adapters for Prometheus, Loki and Tempo are planned, not shipped.
- The question is not about a change ("why has this always been slow?"): Spoor compares against a baseline, so a
  problem that was already there looks normal (it will show under `steady_errors` if it logs errors).
