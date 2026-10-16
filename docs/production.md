# Running Spoor in production

Spoor is a single static binary with no state and no database. Production means: run it as a long-lived HTTP
service near your backends, protect it with a token, give it read-only credentials, and watch its own metrics.

## Choose a shape

| Shape | Good for | Files |
| --- | --- | --- |
| **stdio per developer** | Individuals using Claude Code / Cursor against a reachable stack | none; the MCP client starts `spoor serve` |
| **Kubernetes** | Teams and agent platforms already on Kubernetes | [`deploy/kubernetes/`](../deploy/kubernetes/) |
| **Docker Compose** | A monitoring VM running the Victoria stack in Docker | [`deploy/docker/`](../deploy/docker/) |
| **systemd** | A plain VM | [`deploy/systemd/spoor.service`](../deploy/systemd/spoor.service) |

A shared HTTP instance is the usual choice once more than one person or agent uses Spoor: one place to configure
queries, one set of backend credentials, one place to watch usage.

## Kubernetes

The manifests run two replicas of the distroless image as a non-root user with a read-only root filesystem, no
service-account token, dropped capabilities and `/healthz` probes. They are plain YAML wired together with
Kustomize, and were validated with `kubeconform -strict`.

```sh
kubectl create namespace monitoring      # if it doesn't exist; or change namespace: in kustomization.yaml

# 1. The token clients must send, plus backend tokens if your backends need them.
kubectl -n monitoring create secret generic spoor-secrets \
  --from-literal=SPOOR_HTTP_TOKEN="$(openssl rand -hex 32)"
#   --from-literal=SPOOR_METRICS_TOKEN=… --from-literal=SPOOR_LOGS_TOKEN=… --from-literal=SPOOR_TRACES_TOKEN=…

# 2. Point the config at your backends: edit deploy/kubernetes/configmap.yaml
#    (metrics/logs/traces URLs, ui_url for evidence links, ignore_services, your query recipes).

# 3. Pin a release tag in deploy/kubernetes/kustomization.yaml (images: newTag), then:
kubectl apply -k deploy/kubernetes/
kubectl -n monitoring rollout status deploy/spoor
```

Clients connect to `http://spoor.monitoring.svc:8080/mcp` with `Authorization: Bearer <token>`.

Optional pieces, commented out in `kustomization.yaml`:

- `networkpolicy.yaml`: only pods labelled `spoor-client: "true"` and the `monitoring` namespace may reach Spoor.
- `vmservicescrape.yaml`: lets the VictoriaMetrics operator scrape Spoor's `/metrics`.

**Sessions and replicas.** MCP over streamable HTTP keeps a session in the replica that answered `initialize`. The
Service uses `sessionAffinity: ClientIP`; if you put an ingress or gateway in front, enable sticky sessions there
too (or run one replica). Spoor holds nothing else in memory between calls, so replicas can be added or replaced
freely; a client whose replica disappears simply reconnects.

**Exposing it outside the cluster.** Spoor serves plain HTTP. Terminate TLS at your ingress or gateway, keep the
bearer token on, and consider your gateway's own authentication (OIDC, mTLS) in front.

## Docker Compose

```sh
cd deploy/docker
# edit spoor.yaml: backend URLs as seen from the container
SPOOR_HTTP_TOKEN=$(openssl rand -hex 32) docker compose up -d
docker compose logs -f spoor
```

The compose file uses the published image when it can pull it and otherwise builds one from the repo. It maps
`host.docker.internal` to the Docker host, so backends published on host ports are reachable on Linux as well as
Docker Desktop. If the backends run in Docker, attach Spoor to their network instead and use their service names.

## systemd on a VM

From the repo root (or an unpacked release archive, where the binary is `./spoor`):

```sh
sudo install -m 0755 bin/spoor /usr/local/bin/spoor
sudo install -d -m 0755 /etc/spoor
sudo install -m 0644 examples/spoor.yaml /etc/spoor/spoor.yaml     # then edit backend URLs
echo "SPOOR_HTTP_TOKEN=$(openssl rand -hex 32)" | sudo tee /etc/spoor/spoor.env >/dev/null
sudo chmod 0600 /etc/spoor/spoor.env
sudo cp deploy/systemd/spoor.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now spoor
journalctl -u spoor -f
```

The unit runs as a dynamic user, so `spoor.yaml` must be world-readable; keep tokens in `spoor.env` (mode 0600,
read by systemd itself). It has a read-only filesystem view, no new privileges, a system-call filter and only
IPv4/IPv6 sockets, and listens on `127.0.0.1:8080`. Put a reverse proxy with TLS in front, or change the address if
clients connect over a private network.

## Security

**What Spoor can do.** Spoor only calls query endpoints: `/api/v1/query`, `/api/v1/query_range`,
`/api/v1/label/…/values` and `/select/logsql/query`. A test (`TestReadOnly`) fails the build if any other path is
used. It never writes to your backends and has no shell or file access.

**Credentials.**

- Give Spoor **read-only** backend credentials (`SPOOR_*_TOKEN`, or `SPOOR_*_USER` / `SPOOR_*_PASSWORD` for Basic
  auth). VictoriaMetrics and VictoriaLogs enterprise or `vmauth` can scope a token to the select paths. Through
  Grafana's datasource proxy, use a service account with the Viewer role (see
  [Behind Grafana](configuration.md#behind-grafana)).
- Pass tokens through environment variables or a Secret, never in the config file or in Git.
- Always set `SPOOR_HTTP_TOKEN` (or `--auth-token`) when serving HTTP. Spoor compares it in constant time, answers
  `401` otherwise, and logs a warning at startup if no token is set. `/healthz` and `/metrics` stay open for probes
  and scrapers; restrict them with a network policy if needed.

**What reaches the model.** Tool responses go to whatever LLM your agent uses. Spoor sends aggregates, error
patterns and **one example line per error**. Every text field is redacted before it leaves Spoor: emails, JWTs,
bearer tokens, password/secret/API-key values, card-like digit runs, IPv4 addresses, UUIDs, long hex IDs, and
identifiers that mix letters and digits (see [redaction](how-it-works.md#9-redaction)). Redaction is pattern-based:
identifiers made only of letters, names and free text are not caught. Add `redact` rules for your formats, review
what `spoor new-errors` returns on your data before connecting an external model, and drop fields that must never
leave in your log pipeline.

**Scope.** Anyone who can call Spoor can see what changed in every service the backends hold. If teams must not
see each other's services, run one Spoor per team with backends or tokens scoped to that team.

## Sizing

| Resource | Guidance |
| --- | --- |
| CPU | Mostly idle. 50m request; a call spends most of its time waiting on backends. |
| Memory | 64 Mi idle. Attribution makes three span reads per call, each capped at 50,000 rows; budget roughly 256–512 Mi per replica under concurrent use. The Kubernetes limit is 512 Mi. |
| Backend load | Logs: three or four parallel queries per call whatever the number of services (patterns with exact error counts, line totals); the error filter runs once over the window and once over the baseline. Metrics and traces: about 15 small aggregate queries per service, plus three span reads for attribution when something changed. A full scan of 7 services with all signals is about 110 requests; a logs-only scan is about 6. Add `--stats` to any CLI command to measure yours. Text-regex error filters scan more data than field filters. |
| Latency | Typically 1–5 s for one service, longer for all-services scans on busy backends. Every backend request is bounded by `timeout` (default 20 s). |

If your backends are under pressure, keep `max_services` modest, ask agents to pass `service` when they know it,
and put Spoor behind the same `vmauth` rate limits you use for dashboards.

## Monitoring Spoor

**Health:** `GET /healthz` returns `ok`.

**Metrics:** `GET /metrics` (Prometheus text format). Spoor's own series appear after the first tool call:

| Metric | Labels | Meaning |
| --- | --- | --- |
| `spoor_tool_calls_total` | `tool`, `status` (`ok`, `error`) | Tool calls |
| `spoor_tool_duration_seconds` | `tool` | Tool call latency (histogram*) |
| `spoor_findings` | `tool` | Findings returned per call (histogram*) |
| `spoor_backend_requests_total` | `backend` (`metrics`, `logs`, `traces`), `status` | Backend requests: `ok`, the HTTP status code on failure, or `error` (no response) |
| `spoor_backend_request_duration_seconds` | `backend` | Backend latency (histogram*) |
| `go_*`, `process_*` | | Runtime and process metrics |

\* Histograms use VictoriaMetrics `vmrange` buckets (from the
[`VictoriaMetrics/metrics`](https://github.com/VictoriaMetrics/metrics) library), not Prometheus `le` buckets. Query
them in VictoriaMetrics with
`histogram_quantile(0.95, sum(rate(spoor_tool_duration_seconds_bucket[5m])) by (vmrange, tool))`. Prometheus can
scrape them but its `histogram_quantile` does not understand `vmrange`; use the `_sum` / `_count` series for
averages there.

Suggested alerts:

```promql
# Backend errors: Spoor's answers will have gaps in coverage
sum by (backend) (rate(spoor_backend_requests_total{status!="ok"}[10m]))
  / sum by (backend) (rate(spoor_backend_requests_total[10m])) > 0.1

# Tool calls failing
sum(rate(spoor_tool_calls_total{status="error"}[10m])) / sum(rate(spoor_tool_calls_total[10m])) > 0.1
```

**Logs:** `--log-format json` writes one line per tool call to stderr with `tool`, `duration`, `service`, `since`,
`until`, `baseline`, `findings`, `truncated` and a `coverage_<signal>` field for every signal that was not `ok`.
Failed calls are logged at `WARN` with `error`. `--log-level debug` also logs every backend request (backend,
path, query, status, duration).

## Upgrades

Spoor follows semantic versioning from 1.0. Before 1.0, minor versions may change tool output fields; the
[changelog](../CHANGELOG.md) lists every change. Output changes are deliberate: golden tests pin the exact JSON.

- Pin an image tag or binary version; don't run `latest` in production.
- Spoor is stateless, so upgrades are a rolling restart. Clients reconnect.
- The config is strict: a release that renames a key fails at startup with the key's name rather than silently
  ignoring it. Check the changelog first.

## Production checklist

- [ ] `spoor doctor --config …` has no `FAIL`, and every `WARN` is understood
- [ ] `spoor list-services` shows every service with the signals you expect
- [ ] Past incidents and quiet periods replayed with [`bench/replay.py`](../bench/replay.py): incidents found, quiet windows quiet
- [ ] `SPOOR_HTTP_TOKEN` set; tokens in a Secret or env file, not in Git
- [ ] Backend credentials are read-only
- [ ] `ui_url` set so evidence links open for humans
- [ ] Load generators, collectors and synthetic checks in `ignore_services`
- [ ] Image or binary version pinned
- [ ] `/metrics` scraped and the two alerts above in place
- [ ] Output reviewed for data that must not reach your LLM provider; `redact` rules added for your ID formats
