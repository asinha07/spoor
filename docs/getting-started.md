# Getting started

This guide gets Spoor running on your machine, first against synthetic incidents (no existing stack needed), then
against your own backends, then inside an AI agent.

## 1. Install

Pick one. The rest of this guide also uses `jq` (to read the scenario files) and Docker (for local backends).

**From source** (Go 1.25 or newer):

```sh
git clone https://github.com/asinha07/spoor && cd spoor
make build                 # ./bin/spoor and ./bin/spoorgen
./bin/spoor version
export PATH="$PWD/bin:$PATH"   # so later commands can say just `spoor`
```

**With `go install`:**

```sh
go install github.com/asinha07/spoor/cmd/spoor@latest    # into $(go env GOPATH)/bin
```

**Prebuilt binary:** download the archive for your OS and CPU from the
[releases page](https://github.com/asinha07/spoor/releases), unpack it and put `spoor` on your `PATH`.
On macOS, if Gatekeeper blocks it: `xattr -d com.apple.quarantine ./spoor`.

**Docker:**

```sh
docker build -t spoor --build-arg VERSION=$(git describe --tags --always) .
docker run --rm spoor version
docker run -i --rm spoor        # MCP over stdio (the image default)
docker run --rm -p 8080:8080 -e SPOOR_HTTP_TOKEN=… spoor serve --http :8080   # shared HTTP server
```

Released images are published as `ghcr.io/asinha07/spoor:<version>`. If Docker Hub rate-limits the build, add
`--build-arg GO_IMAGE=mirror.gcr.io/library/golang:1.25`.

## 2. Try it on synthetic incidents (about 5 minutes)

Requires Docker. This starts empty VictoriaMetrics, VictoriaLogs and VictoriaTraces containers and loads six
incidents with known answers.

```sh
make it-up        # backends on :8428 (metrics), :9428 (logs), :10428 (traces)
make demo-data    # ~40 hours of telemetry for a 7-service shop, with six incidents
```

`make demo-data` writes the ground truth for each incident to `scenarios/out/<scenario>.truth.json`:

| Scenario | What breaks | Where it starts |
| --- | --- | --- |
| `payment-failure` | 40% of card charges fail; checkout and frontend fail as a result | payment |
| `catalog-one-product` | Lookups of exactly one product ID fail | product-catalog |
| `ad-high-cpu` | The ad service saturates its CPU and slows down | ad |
| `recommendation-cache-leak` | The cache leaks memory; latency creeps up with it | recommendation |
| `kafka-queue-problems` | The accounting consumer falls behind on the orders topic | accounting |
| `control` | Nothing. Only the usual background errors. | — |

Ask Spoor about one (the `./bin/spoor` commands assume you built from source; use `spoor` if it's on your `PATH`):

```sh
T=scenarios/out/payment-failure.truth.json
./bin/spoor what-changed --since $(jq -r .since $T) --until $(jq -r .until $T) --pretty
```

The top finding should be `payment — Failures start here, in "POST /charge"`, with checkout and frontend errors
labelled `— originates in payment`. Try the `control` scenario too: the answer should be "No significant change",
with the background errors listed under `steady_errors`.

Other commands:

```sh
./bin/spoor list-services --since 40h --pretty
./bin/spoor new-errors --service payment --since $(jq -r .since $T) --until $(jq -r .until $T) --pretty
./bin/spoor trace-diff --service payment --since $(jq -r .since $T) --until $(jq -r .until $T) --pretty
```

Each finding can carry an `evidence_link` to the underlying query in VMUI. Links are the first thing dropped to fit
the token budget, so at the default 800 tokens you may see none; add `--budget 4000` to keep them, then open one in
a browser.

When you are done: `make it-down` (stops the containers and deletes their data).

## 3. Point it at your own backends

```sh
export SPOOR_METRICS_URL=http://victoriametrics:8428
export SPOOR_LOGS_URL=http://victorialogs:9428
export SPOOR_TRACES_URL=http://victoriatraces:10428
# Behind Grafana instead? See "Behind Grafana" in configuration.md.

spoor doctor --since 15m
```

**Prometheus, Loki, Tempo or New Relic?** Set each signal's type; the defaults follow that backend:

```sh
# Grafana stack
export SPOOR_METRICS_TYPE=prometheus SPOOR_METRICS_URL=http://prometheus:9090
export SPOOR_LOGS_TYPE=loki SPOOR_LOGS_URL=http://loki:3100
export SPOOR_TRACES_TYPE=tempo SPOOR_TRACES_URL=http://tempo:3200

# New Relic (a User key, NRAK-...)
export SPOOR_METRICS_TYPE=newrelic SPOOR_LOGS_TYPE=newrelic SPOOR_TRACES_TYPE=newrelic
export NEW_RELIC_ACCOUNT_ID=1234567 NEW_RELIC_API_KEY=NRAK-...

spoor doctor --since 15m
```

Ready-made configs: [`examples/grafana-stack.yaml`](../examples/grafana-stack.yaml),
[`examples/newrelic.yaml`](../examples/newrelic.yaml). What differs per backend: [backends.md](backends.md).

`doctor` reads a sample of each backend and reports what Spoor can and cannot see, then suggests a config. On
logs that don't follow OpenTelemetry naming, expect something like:

```
logs
       5354 log lines, 7 fields
       service field candidate container: on 100% of lines, 5 distinct values
  WARN service field service.name is on only 0% of lines; container is on 100%
       severity field candidate level: on 100.0% of lines, values INFO, ERROR
       severity field candidate log.level: on 100.0% of lines, values INFO, ERROR
  WARN 12 of those lines carry a level in their text (e.g. "... [ERROR] ..."); add a text rule to error_filter
  FAIL the configured error filter matches 0 lines: Spoor sees no log errors (and would report no change)

Suggested config (review before use):

logs:
  service_field: container
  severity_fields: [level, log.level]
  error_filter: '(… OR (NOT level:* NOT log.level:* _msg:~"…"))'
```

Put the suggestions (and your metric names, if they differ from the OpenTelemetry defaults) in a config file and run
`doctor` again until it has no `FAIL` and you understand every `WARN`. The
[configuration guide](configuration.md) has recipes for non-OpenTelemetry logs, error messages full of IDs, span
metrics, classic Prometheus naming, cAdvisor and Grafana.

```sh
spoor doctor --config spoor.yaml --since 15m
spoor list-services --config spoor.yaml --since 30m --pretty
spoor what-changed --config spoor.yaml --since 30m --pretty --stats
```

Check the output:

- **Every service you expect is listed**, with the signals you send.
- **`coverage` says `ok`** for each backend, and `coverage.logs_lines` shows a believable number of error lines.
  Any other state means Spoor is partly blind there; the summary says so too.
- **On a healthy system** you should see few or no findings, maybe some `steady_errors`. If you see many findings on
  a quiet day, see [troubleshooting.md](troubleshooting.md#too-many-findings).

Before relying on it, replay windows you already understand: past incidents and quiet periods. See
[Replay your own incidents](benchmark.md#replay-your-own-incidents).

## 4. Connect an AI agent

Spoor speaks MCP. Most clients launch it as a subprocess over stdio.

**Claude Code:**

```sh
claude mcp add spoor \
  -e SPOOR_METRICS_URL=http://localhost:8428 \
  -e SPOOR_LOGS_URL=http://localhost:9428 \
  -e SPOOR_TRACES_URL=http://localhost:10428 \
  -- /absolute/path/to/spoor serve
# with a config file:
claude mcp add spoor -- /absolute/path/to/spoor serve --config /absolute/path/to/spoor.yaml
```

By default `claude mcp add` registers the server for the current project directory only. Add `-s user`
(`claude mcp add -s user spoor …`) to make it available in every directory. `claude mcp list` shows whether it
connected.

Then ask, for example: *"Checkout errors went up in the last 30 minutes. What changed, and where did it start?"*

**Codex:**

```sh
codex mcp add spoor \
  --env SPOOR_METRICS_URL=http://localhost:8428 \
  --env SPOOR_LOGS_URL=http://localhost:9428 \
  --env SPOOR_TRACES_URL=http://localhost:10428 \
  -- /absolute/path/to/spoor serve
# with a config file:
codex mcp add spoor --env SPOOR_CONFIG=/absolute/path/to/spoor.yaml -- /absolute/path/to/spoor serve
```

Or edit `~/.codex/config.toml` directly. Use full paths (`~` is not expanded there), and raise the tool timeout:
a scan of every service on a busy system can take half a minute, longer than Codex waits by default.

```toml
[mcp_servers.spoor]
command = "/absolute/path/to/spoor"
args = ["serve"]
tool_timeout_sec = 120

[mcp_servers.spoor.env]
SPOOR_CONFIG = "/absolute/path/to/spoor.yaml"
```

`codex mcp list` shows it; `/mcp` inside Codex lists the four tools.

**Cursor, Claude Desktop, Windsurf and other clients** take a JSON config
([`examples/mcp.json`](../examples/mcp.json)). For Cursor, put it in `.cursor/mcp.json`; for Claude Desktop, in
`claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "spoor": {
      "command": "/absolute/path/to/spoor",
      "args": ["serve", "--config", "/absolute/path/to/spoor.yaml"],
      "env": {
        "SPOOR_METRICS_URL": "http://localhost:8428",
        "SPOOR_LOGS_URL": "http://localhost:9428",
        "SPOOR_TRACES_URL": "http://localhost:10428"
      }
    }
  }
}
```

Use absolute paths: MCP clients do not start servers from your shell, so `PATH` and the working directory may differ.

**A shared Spoor over HTTP** (one instance for a team; see [production.md](production.md)):

```sh
export SPOOR_HTTP_TOKEN=$(openssl rand -hex 32)   # or pass --auth-token
spoor serve --http :8080                          # MCP endpoint: http://localhost:8080/mcp

# from a client (replace localhost with the server's address when it runs elsewhere):
claude mcp add --transport http spoor http://localhost:8080/mcp \
  --header "Authorization: Bearer $SPOOR_HTTP_TOKEN"
```

### Using Spoor alongside other tools

Spoor is meant to be the **first** call, not the only one. Keep your raw query tools (for example the official
VictoriaMetrics MCP servers) connected too: Spoor tells the agent where to look, the raw tools let it look closer.
That combination is what the [benchmark](benchmark.md) measures.

## 5. Try it on a real microservice app

The [OpenTelemetry Demo guide](../deploy/demo/README.md) runs the OpenTelemetry Astronomy Shop (about 20 services)
with its telemetry going to the Victoria stack, injects real faults with feature flags, and shows what Spoor
reports. It needs about 8 GB of memory for Docker and 30 minutes of warm-up.

## Next

- [Production deployment](production.md)
- [How Spoor decides what changed](how-it-works.md)
- [Tool reference](tools.md)
