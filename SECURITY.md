# Security policy

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private vulnerability reporting (the repository's **Security** tab →
**Report a vulnerability**) with a description, steps to reproduce and the affected version. You will get an
acknowledgement within 72 hours.

## Design notes

- Spoor is read-only: it only calls query endpoints of VictoriaMetrics, VictoriaLogs and VictoriaTraces.
- Bearer tokens are read from config or environment and sent only to the backend they belong to.
- Log examples are redacted (emails, JWTs, bearer tokens, password/secret/API-key values, card-like numbers, UUIDs, long hex strings; not IP
  addresses) before they are
  returned to an agent. Redaction is best-effort; do not rely on it as your only control for sensitive logs.
- `spoor serve --http` requires a bearer token when `--auth-token` / `SPOOR_HTTP_TOKEN` is set (compared in constant
  time) and warns at startup when it is not. It serves plain HTTP: terminate TLS in front of it.
- Production guidance (credentials, network policy, what reaches the model): [docs/production.md](docs/production.md#security).
