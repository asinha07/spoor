# Running Spoor against the OpenTelemetry Demo

> **Status: verified** on OpenTelemetry Demo v3.1.0 (Docker Desktop, macOS arm64, 8 GB for Docker), October 2026.

The [OpenTelemetry Demo](https://github.com/open-telemetry/opentelemetry-demo) (the "Astronomy Shop") is a real
microservices app with built-in fault-injection flags. This setup sends its telemetry to VictoriaMetrics,
VictoriaLogs and VictoriaTraces instead of the demo's own Jaeger/Prometheus/OpenSearch stack, which also saves
memory.

## Results from the verification run

| Fault (flag) | Spoor's top finding | Echoes handled | Tokens |
| --- | --- | --- | --- |
| `paymentFailure` at 50% | payment: "Failures start here, in `charge` (100% of 16 failed traces)", error rate 0% → 64%, onset 13 s after injection | checkout and frontend errors labelled "originates in payment" | 732 |
| `adHighCpu` | ad: "CPU up: 0.07 cores → 3.97 cores" (the only finding) | — | 192 |

Two Spoor bugs were found and fixed on the way. Both are covered by unit tests now:
- **Error origin across an unmarked span.** The demo's frontend returns errors without marking its own span failed.
  Spoor used to treat the Envoy proxy above it as a second origin and blamed it instead of payment. Origins are now
  found by walking the whole trace tree.
- **Ratio-unit CPU.** `container_cpu_utilization_ratio` is a share of *all* host cores, so a saturated core read as a
  few percent. The config below uses cores instead.

## 1. Get the demo

```sh
git clone --depth 1 https://github.com/open-telemetry/opentelemetry-demo
cd opentelemetry-demo
```

## 2. Add the Victoria backends

```sh
SPOOR=/path/to/spoor
cp $SPOOR/deploy/demo/compose.victoria.yaml .
cp $SPOOR/deploy/demo/otelcol-config-extras.yml src/otel-collector/otelcol-config-extras.yml   # the demo's copy is empty by design
cp $SPOOR/deploy/demo/spoor.yaml $SPOOR/deploy/demo/flag.sh .
touch .env.override
```

If something on your machine already uses port 8080, move the demo's frontend proxy:

```sh
printf 'ENVOY_PORT=18090\nFRONTEND_PROXY_ADDR=frontend-proxy:18090\nLOCUST_HOST=http://frontend-proxy:18090\n' > .env.override
```

## 3. Start it

```sh
docker compose --env-file .env --env-file .env.override \
  -f compose.yaml -f compose.full.yaml -f compose.victoria.yaml up -d
```

First start pulls about 8 GB of images. Give it **at least 30 minutes** of normal traffic before injecting a fault:
Spoor compares against four baseline windows before the one you ask about, and the load generator ramps up users
over the first 20–30 minutes.

## 4. Check Spoor sees everything

```sh
spoor list-services --config spoor.yaml --since 10m --pretty   # ~21 services, each with metrics+logs+traces
spoor what-changed --config spoor.yaml --since 5m --pretty      # quiet system: few or no findings
```

## 5. Inject a fault and ask

```sh
./flag.sh paymentFailure 50%      # prints the fault start time
# wait 5 minutes
spoor what-changed --config spoor.yaml --since <fault start time> --pretty
./flag.sh paymentFailure off      # flagd applies it within about a minute
```

`flag.sh` accepts `on`, `off` or any variant the flag defines (`./flag.sh paymentFailure 37%` lists the valid ones).
Useful flags: `paymentFailure`, `productCatalogFailure`, `adHighCpu`, `recommendationCacheFailure`,
`kafkaQueueProblems`, `cartFailure`, `emailMemoryLeak`.

## 6. Tear down

```sh
docker compose --env-file .env --env-file .env.override \
  -f compose.yaml -f compose.full.yaml -f compose.victoria.yaml down -v
# Free the ~8 GB of demo images:
docker images --format '{{.Repository}}:{{.Tag}}' | grep '^ghcr.io/open-telemetry/demo:' | xargs docker rmi
```

## What the config does

- **Metrics:** the demo's services use three different request-metric conventions, so request rate, error rate and
  p95 come from the collector's span metrics (`traces_span_metrics_*`), which cover every service the same way. CPU
  (cores) and memory come from the `docker_stats` receiver, keyed by container name.
- **Logs and traces:** Spoor's defaults work unchanged.
- **`ignore_services`:** the load generator, browser client, collector, docs and flagd are traffic sources and
  infrastructure, so they are left out of scans. You can still query them by name.

Note: the collector's `docker_stats` receiver reports **every** container on your Docker host, not only the demo's.
That data stays in your local VictoriaMetrics.
