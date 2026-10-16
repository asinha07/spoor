#!/usr/bin/env bash
# Turn an OpenTelemetry Demo feature flag on or off by editing the flagd
# config, which flagd hot-reloads. Run from the opentelemetry-demo directory.
#
#   flag.sh <flag> on|off|<variant>     e.g. flag.sh paymentFailure 50%
#
# Scenario mapping (flag names differ between demo releases; check
# src/flagd/demo.flagd.json):
#   payment-failure            paymentFailure
#   catalog-one-product        productCatalogFailure
#   ad-high-cpu                adHighCpu
#   recommendation-cache-leak  recommendationCacheFailure
#   kafka-queue-problems       kafkaQueueProblems
set -euo pipefail
flag=${1:?flag name}
state=${2:?on, off or a variant name}
file=${FLAGD_FILE:-src/flagd/demo.flagd.json}
[ -f "$file" ] || { echo "not found: $file (run from the opentelemetry-demo directory)" >&2; exit 1; }
jq -e --arg f "$flag" '.flags[$f]' "$file" >/dev/null || { echo "unknown flag $flag; available:" >&2; jq -r '.flags | keys[]' "$file" >&2; exit 1; }
variant=$state
if [ "$state" != on ] && [ "$state" != off ] && ! jq -e --arg f "$flag" --arg v "$state" '.flags[$f].variants | has($v)' "$file" >/dev/null; then
  echo "unknown variant $state for $flag; available: $(jq -r --arg f "$flag" '.flags[$f].variants | keys | join(", ")' "$file")" >&2; exit 1
fi
# Most flags use on/off variants; numeric flags use their highest variant for "on".
if [ "$state" = on ] && ! jq -e --arg f "$flag" '.flags[$f].variants.on' "$file" >/dev/null; then
  variant=$(jq -r --arg f "$flag" '.flags[$f].variants | to_entries | max_by(.value) | .key' "$file")
fi
tmp=$(mktemp)
jq --arg f "$flag" --arg v "$variant" '.flags[$f].defaultVariant = $v' "$file" > "$tmp" && mv "$tmp" "$file"
echo "$flag -> $variant ($(date -u +%Y-%m-%dT%H:%M:%SZ)); note this time as the fault start"
