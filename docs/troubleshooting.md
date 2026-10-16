# Troubleshooting

Start with `spoor list-services --since 30m --pretty` and look at `coverage`. Most problems show up there.

## Coverage says `error: …`

Spoor could not reach a backend, or the backend rejected a query.

- `backend unavailable: … connection refused`: wrong URL or port, or the backend is down. Check
  `SPOOR_*_URL` and try `curl <url>/health`.
- `… 401` / `403`: the backend needs a token. Set `SPOOR_METRICS_TOKEN`, `SPOOR_LOGS_TOKEN` or `SPOOR_TRACES_TOKEN`.
- `… 400` with a query error: one of your query templates is invalid for your data. Run it in VMUI with a real
  service name and window substituted.
- `context deadline exceeded`: the backend is slow. Raise `timeout`, narrow the window, or pass `service`.

The other signals still answer; only the failing one is missing.

## A service is missing from `list_services`

- **Missing from metrics:** Spoor lists services from the values of `service_label` (default `service_name`). Check
  the label exists on your request metric: `count by (service_name) (http_server_request_duration_seconds_count)`.
- **Missing from logs or traces:** check `service_field` (default `service.name` for logs,
  `resource_attr:service.name` for traces). In VMUI, open a log line and look for the field that holds the service.
- **Names differ between signals** (`checkout` in metrics, `checkoutservice` in logs): Spoor sees two services, and
  cross-signal ranking and attribution suffer. Fix the naming at the collector (`service.name` resource attribute)
  so all three signals agree.

## "No significant change", but something is clearly wrong

1. **Read `coverage`.** `no data in baseline period` means there was nothing to compare with: use
   `baseline=previous` or a window your backends retain. `no data for <service>` means the service isn't sending
   that signal.
2. **Check the window.** The window must include the problem, and the baseline must not. If the problem started
   two hours ago, `since=30m` compares a broken window with a broken baseline. Use `--since` with the time it
   started.
3. **Problems that were always there are not changes.** Look at `steady_errors`.
4. **Low traffic.** Services below `min_request_rate` skip error-rate and latency scoring. Lower it if your
   services are quiet.
5. **Lower the threshold** to see near misses: `SPOOR_REPORT_THRESHOLD=0.3 spoor what-changed …`.

## Too many findings

- Raise `report_threshold` (0.6 or 0.7) or `min_pattern_count`.
- Add load generators, collectors and synthetic checks to `ignore_services`.
- Use more baseline history: a larger `baseline_sub_windows` learns normal variation better.
- Very short windows (a few minutes) are noisy; 15–30 minutes is the sweet spot.

## The wrong service is named as the origin

Attribution follows trace parent/child links. It goes wrong when traces are broken:

- Context propagation is missing between two services, so their spans land in separate traces.
- A proxy or gateway reports an error status without the backend's span below it.
- Very high volume: each attribution read is capped at 50,000 span rows, so a trace can be cut. Narrow the window.

Check with the `trace_id` on a span finding: open it in VMUI and see whether the tree is complete.

## CPU findings look too small

If you use a utilisation ratio such as `container_cpu_utilization_ratio`, a fully busy core on a 16-core host is
about 6%. Use cores instead: `sum(rate(container_cpu_usage_seconds_total{…}[{{.Window}}]))` with `unit: cores`.

## Config errors at startup

- `field xyz not found in type config.…`: unknown key, usually a typo or wrong nesting. Spoor rejects unknown keys
  on purpose. Compare with [`examples/spoor.yaml`](../examples/spoor.yaml).
- `metrics.request_rate: template: …`: the template doesn't parse. Use `{{.Service}}` and `{{.Window}}` exactly.

## The MCP client doesn't see Spoor's tools

- Use absolute paths to the binary and the config in the client's config. Clients don't start servers from your
  shell.
- Run the exact command yourself: `/path/to/spoor serve --config /path/to/spoor.yaml` should start without errors
  and wait for MCP messages on stdin. A config or flag problem prints `spoor: …` and exits. Press Ctrl-C to stop.
- Claude Code: `claude mcp list` shows whether the server connected.
- HTTP: `curl -i http://host:8080/healthz` should return `ok`. A `401` on `/mcp` means the client isn't sending
  `Authorization: Bearer <token>`.
- Behind a load balancer, `Session not found` errors mean requests from one client reach different replicas. Turn on
  sticky sessions.

## Evidence links don't open

Links point at `ui_url` (or `url` when unset). If Spoor reaches the backends on internal addresses, set `ui_url` to
the address people use in a browser.

## Still stuck

Run `spoor serve --log-level debug` to log every backend query with its status and duration, and open an issue with the command, the `coverage` block and your config (remove
tokens and URLs you consider private).
