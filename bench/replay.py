#!/usr/bin/env python3
"""Replay labelled time windows through Spoor and grade the answers.

Use it on your own history: past incidents you know the cause of, and quiet
windows where nothing was wrong. It measures what matters before trusting
Spoor in production: incidents caught (top 1 / top 3), incidents missed,
findings on quiet windows (false alarms), latency, response size and backend
load. It only reads, through the spoor binary, with your config.

    python3 bench/replay.py windows.csv --config spoor.yaml
    python3 bench/replay.py windows.csv --config spoor.yaml --out replay-results

windows.csv columns (header required; keep this file out of git):

    name,since,until,expected_service,expected_kind,notes
    checkout-outage-0612,2026-06-12T09:40:00Z,2026-06-12T10:10:00Z,payment,,card processor down
    quiet-tuesday,2026-06-17T14:00:00Z,2026-06-17T14:30:00Z,none,,normal traffic

expected_service is the service where the problem started, or "none" for a
quiet window. expected_kind (optional) is a finding kind that should appear
for that service, e.g. new_log_pattern or error_rate_up.
"""
import argparse
import csv
import json
import os
import re
import statistics
import subprocess
import sys
import time

STATS = re.compile(r"spoor: (\d+) backend requests, ([\d.]+)s waiting on backends")


def run_one(args, row):
    cmd = [args.spoor, "what-changed", "--since", row["since"], "--until", row["until"],
           "--budget", str(args.budget), "--stats"]
    if args.config:
        cmd += ["--config", args.config]
    if args.baseline:
        cmd += ["--baseline", args.baseline]
    started = time.monotonic()
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=args.timeout)
    wall = time.monotonic() - started
    result = {"name": row["name"], "seconds": round(wall, 2)}
    m = STATS.search(p.stderr)
    if m:
        result["backend_requests"] = int(m.group(1))
    if p.returncode != 0:
        result["error"] = p.stderr.strip()[-300:]
        return result, None
    env = json.loads(p.stdout)
    result["tokens"] = len(p.stdout.encode()) // 4
    result["findings"] = len(env.get("findings", []))
    result["coverage_issues"] = {k: v for k, v in env.get("coverage", {}).items()
                                 if k in ("metrics", "logs", "traces") and v not in ("ok", "not configured")}
    return result, env


def grade(row, result, env):
    expected = (row.get("expected_service") or "").strip()
    kind = (row.get("expected_kind") or "").strip()
    if env is None:
        result["verdict"] = "error"
        return
    services = []
    for f in env.get("findings", []):
        if f["service"] not in services:
            services.append(f["service"])
    result["top_services"] = services[:3]
    if expected.lower() in ("", "none"):
        result["verdict"] = "quiet-ok" if not services else "false-alarm"
        return
    if not services or expected not in services:
        result["verdict"] = "missed"
    elif services[0] == expected:
        result["verdict"] = "top1"
    elif expected in services[:3]:
        result["verdict"] = "top3"
    else:
        result["verdict"] = "found-lower"
    if kind:
        kinds = {f["kind"] for f in env.get("findings", []) if f["service"] == expected}
        result["kind_found"] = kind in kinds


def summarize(rows, results):
    faults = [r for row, r in zip(rows, results) if (row.get("expected_service") or "").lower() not in ("", "none")]
    quiet = [r for row, r in zip(rows, results) if (row.get("expected_service") or "").lower() in ("", "none")]
    count = lambda rs, v: sum(1 for r in rs if r.get("verdict") == v)
    lines = ["# Spoor replay", "",
             "| Window | Expected | Verdict | Top services | Findings | Tokens | Backend requests | Seconds | Coverage issues |",
             "| --- | --- | --- | --- | --- | --- | --- | --- | --- |"]
    for row, r in zip(rows, results):
        issues = "; ".join(f"{k}: {v[:60]}" for k, v in (r.get("coverage_issues") or {}).items())
        lines.append("| {} | {} | {} | {} | {} | {} | {} | {} | {} |".format(
            r["name"], row.get("expected_service") or "none", r.get("verdict"),
            ", ".join(r.get("top_services", [])), r.get("findings", ""), r.get("tokens", ""),
            r.get("backend_requests", ""), r.get("seconds"), issues or (r.get("error", "")[:80])))
    lines.append("")
    if faults:
        lines.append(f"- Incidents: {len(faults)}. Caught as #1: {count(faults, 'top1')}, in top 3: "
                     f"{count(faults, 'top1') + count(faults, 'top3')}, missed: {count(faults, 'missed')}, "
                     f"errors: {count(faults, 'error')}.")
    if quiet:
        lines.append(f"- Quiet windows: {len(quiet)}. No findings: {count(quiet, 'quiet-ok')}, "
                     f"false alarms: {count(quiet, 'false-alarm')}.")
    ok = [r for r in results if "seconds" in r and "error" not in r]
    if ok:
        lines.append(f"- Median latency {statistics.median(r['seconds'] for r in ok):.2f}s, median response "
                     f"{statistics.median(r.get('tokens', 0) for r in ok):.0f} tokens, median backend requests "
                     f"{statistics.median(r.get('backend_requests', 0) for r in ok):.0f}.")
    lines.append("")
    lines.append("Windows labelled after looking at Spoor's output are not a fair test: label first, then replay.")
    return "\n".join(lines) + "\n"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("windows", help="CSV of labelled windows")
    ap.add_argument("--spoor", default=os.path.join(os.path.dirname(__file__), "..", "bin", "spoor"))
    ap.add_argument("--config", help="spoor config file")
    ap.add_argument("--baseline", help="previous (default), 1d or 7d")
    ap.add_argument("--budget", type=int, default=800)
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument("--out", help="directory for summary.md and results.json (default: print summary only)")
    args = ap.parse_args()

    with open(args.windows, newline="") as fh:
        rows = [r for r in csv.DictReader(fh) if (r.get("name") or "").strip() and not r["name"].startswith("#")]
    if not rows:
        sys.exit("no windows in " + args.windows)
    results = []
    for row in rows:
        try:
            r, env = run_one(args, row)
        except subprocess.TimeoutExpired:
            r, env = {"name": row["name"], "error": "timeout", "seconds": args.timeout}, None
        grade(row, r, env)
        results.append(r)
        print(f"{r['name']}: {r.get('verdict')} ({r.get('seconds')}s)", file=sys.stderr)
    summary = summarize(rows, results)
    if args.out:
        os.makedirs(args.out, exist_ok=True)
        with open(os.path.join(args.out, "summary.md"), "w") as fh:
            fh.write(summary)
        with open(os.path.join(args.out, "results.json"), "w") as fh:
            json.dump(results, fh, indent=2)
    print(summary)


if __name__ == "__main__":
    main()
