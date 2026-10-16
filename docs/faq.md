# FAQ

**Does Spoor send my data anywhere?**
No. Spoor talks only to the backends you configure, and only to their query endpoints. Its answers go to whoever
called it; if that is an agent using a hosted LLM, the answers reach that provider. See
[production.md § Security](production.md#security) for what the answers contain.

**Does Spoor use an LLM?**
No. It is statistics and trace walking in Go. The same data always gives the same answer, which is also why it is
cheap and fast.

**Why not just give the agent PromQL / LogsQL access?**
You should, as well. But an agent with only raw queries spends many calls deciding what to query, comparing with
a baseline and separating causes from echoes, and often stops at the loudest symptom. In the
[benchmark](benchmark.md), giving it Spoor as well cut tool calls by more than half and raised accuracy.

**Which agents and clients work with it?**
Any MCP client: Claude Code, Claude Desktop, Cursor, Windsurf, VS Code with MCP, agent frameworks with MCP support.
Humans can use the CLI.

**Do I need all three signals?**
No. Each signal works on its own and `coverage` says which answered. Traces are what let Spoor tell a cause from
its echoes; without them you still get ranked metric and log changes.

**Does it work with Prometheus, Loki, Tempo, Elasticsearch, Datadog?**
Not officially yet. The metrics side uses the standard Prometheus HTTP query API and the default queries are valid
PromQL, so pointing `metrics.url` at Prometheus or a Prometheus-compatible store may work, but it is untested and
evidence links assume VMUI. Logs and traces need VictoriaLogs and VictoriaTraces. Prometheus, Loki and Tempo
adapters are the next milestone.

**Do I need OpenTelemetry?**
No, but the defaults assume OTel names. Other naming is a config change: see the
[configuration recipes](configuration.md#recipes).

**How big is the baseline? How much history do I need?**
With the default `baseline=previous` and six sub-windows, a 30-minute question needs 3.5 hours of history. `1d`
and `7d` need a day or a week of retention. If the baseline is empty, Spoor tells you instead of guessing.

**What does `score` mean?**
0 to 1. 0.5, the default reporting threshold, corresponds to a robust z-score of 3 against the baseline. Scores
above 0.9 are strong changes, usually confirmed by more than one signal. See [how-it-works.md](how-it-works.md).

**What are `steady_errors`?**
Services whose errors run at their usual rate. They were there before the window, so they probably don't explain a
new problem. Agents tend to blame any error they see; this field tells them not to.

**Why is the answer cut off? What is `truncated`?**
Every tool fits its answer into `budget_tokens` (default 800). `truncated` counts findings that didn't fit. Ask
again with a larger budget, or with `service` set.

**Can Spoor change anything in my systems?**
No. It has no write paths, no shell, no file access. `TestReadOnly` fails the build if code ever calls a non-query
endpoint.

**How do I add support for my metric names / a new backend / a new scorer?**
Names: config. Backends and scorers: see [CONTRIBUTING.md](../CONTRIBUTING.md) and
[architecture.md](architecture.md).

**Is the benchmark fair? You wrote the scenarios.**
A fair worry. The synthetic incidents were written by Spoor's author, so the benchmark also includes a no-fault
control, both arms get the same raw tools, and the prompt never names a service. Spoor has also been checked on
the independent [OpenTelemetry Demo](../deploy/demo/README.md). Better external benchmarks are welcome.

**Why "Spoor"?**
Spoor is the track an animal leaves. Spoor follows the trail a failure leaves in your telemetry back to where it
started.
