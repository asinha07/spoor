#!/usr/bin/env python3
"""Grade benchmark results against ground truth (see rubric.md).

usage: grade.py RESULTS_DIR TRUTH_DIR  > summary.md
"""
import glob
import json
import os
import re
import statistics
import sys

KEYWORDS = {
    "payment-failure": ["charge", "card", "processor", "503", "payment fail", "declin"],
    "catalog-one-product": ["oljcespc7z", "one product", "single product", "specific product", "product id", "feature flag"],
    "ad-high-cpu": ["cpu"],
    "recommendation-cache-leak": ["memory", "leak", "cache"],
    "kafka-queue-problems": ["lag", "kafka", "consumer", "queue", "backlog"],
}
NONE_WORDS = ["none", "no problem", "no root cause"]


def root_cause_line(text):
    lines = [l for l in (text or "").splitlines() if l.strip().upper().startswith("ROOT CAUSE:")]
    return lines[-1].strip() if lines else ""


def grade(scenario, truth, text):
    """Returns (correct, service_ok, line)."""
    line = root_cause_line(text)
    low = line.lower()
    if not line:
        return False, False, ""
    svc = (truth.get("service") or "").lower()
    if not svc:  # control
        ok = any(w in low for w in NONE_WORDS)
        return ok, ok, line
    # The service is what comes before the dash; the rest is the mechanism.
    # "checkout — errors calling payment" names checkout, not payment.
    named = re.split(r"\s[—–-]\s", low.split(":", 1)[1], maxsplit=1)[0]
    # Match the service as a whole word so "ad" does not match "load".
    service_ok = re.search(r"(?<![a-z0-9-])" + re.escape(svc) + r"(?![a-z0-9-])", named) is not None
    mech_ok = any(k in low for k in KEYWORDS.get(scenario, []))
    return service_ok and mech_ok, service_ok, line


def tokens(r):
    u = r.get("usage") or {}
    return sum(u.get(k, 0) or 0 for k in ("input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"))


def main():
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    results_dir, truth_dir = sys.argv[1], sys.argv[2]
    rows = []
    for path in sorted(glob.glob(os.path.join(results_dir, "*.*.*.json"))):
        name = os.path.basename(path)[: -len(".json")]
        scenario, arm, run = name.rsplit(".", 2)
        truth_path = os.path.join(truth_dir, scenario + ".truth.json")
        if not os.path.exists(truth_path):
            continue
        with open(truth_path) as f:
            truth = json.load(f)
        with open(path) as f:
            try:
                r = json.load(f)
            except json.JSONDecodeError:
                r = {}
        correct, service_ok, line = grade(scenario, truth, r.get("result", ""))
        rows.append({
            "scenario": scenario, "arm": arm, "run": run, "correct": correct, "service_ok": service_ok,
            "tokens": tokens(r), "turns": r.get("num_turns") or 0, "cost": r.get("total_cost_usd") or 0.0,
            "seconds": r.get("bench_wall_seconds") or 0, "line": line,
            "spoor_calls": sum(v for k, v in (r.get("tool_calls") or {}).items() if k.startswith("mcp__spoor__")),
            "tool_calls": sum((r.get("tool_calls") or {}).values()),
        })
    if not rows:
        sys.exit("no results found")

    def med(xs):
        return statistics.median(xs) if xs else 0

    print("| Scenario | Arm | Runs | Correct | Service right | Median tool calls | Used Spoor | Median turns | Median cost | Median seconds |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    groups = {}
    for r in rows:
        groups.setdefault((r["scenario"], r["arm"]), []).append(r)
    for (sc, arm), g in sorted(groups.items()):
        print(f"| {sc} | {arm} | {len(g)} | {sum(x['correct'] for x in g)}/{len(g)} | {sum(x['service_ok'] for x in g)}/{len(g)} | "
              f"{med([x['tool_calls'] for x in g]):.0f} | {sum(x['spoor_calls'] > 0 for x in g)}/{len(g)} | {med([x['turns'] for x in g]):.0f} | "
              f"${med([x['cost'] for x in g]):.3f} | {med([x['seconds'] for x in g]):.0f} |")
    print()
    for arm in sorted({r["arm"] for r in rows}):
        g = [r for r in rows if r["arm"] == arm]
        print(f"- Arm {arm}: {sum(x['correct'] for x in g)}/{len(g)} correct, {sum(x['service_ok'] for x in g)}/{len(g)} right service, "
              f"median {med([x['tool_calls'] for x in g]):.0f} tool calls, median {med([x['tokens'] for x in g]):,.0f} tokens (incl. cache reads), "
              f"total cost ${sum(x['cost'] for x in g):.2f}")
    print("\n<details><summary>Final lines</summary>\n")
    for r in rows:
        mark = "✔" if r["correct"] else "✘"
        print(f"- {mark} {r['scenario']} {r['arm']}#{r['run']}: {r['line'] or '(no ROOT CAUSE line)'}")
    print("\n</details>")


if __name__ == "__main__":
    main()
