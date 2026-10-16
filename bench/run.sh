#!/usr/bin/env bash
# Runs the Spoor benchmark: each scenario × arm × run is one headless Claude
# Code session. Results land in bench/results/<scenario>.<arm>.<run>.json.
#
#   RUNS=5 ARMS="a b" SCENARIOS="payment-failure control" MAX_USD=2 TOTAL_USD=5 MODEL=haiku ./bench/run.sh
set -euo pipefail

cd "$(dirname "$0")/.."
RUNS=${RUNS:-5}
ARMS=${ARMS:-"a b"}
TRUTH_DIR=${TRUTH_DIR:-scenarios/out}
SCENARIOS=${SCENARIOS:-$(ls "$TRUTH_DIR"/*.truth.json 2>/dev/null | xargs -n1 basename | sed 's/\.truth\.json$//' | tr '\n' ' ')}
MAX_USD=${MAX_USD:-2}
TOTAL_USD=${TOTAL_USD:-}   # optional hard stop on total spend across all results
MODEL=${MODEL:-}
OUT=bench/results
PWD_ABS=$PWD

for cmd in claude jq; do
  command -v "$cmd" >/dev/null || { echo "missing: $cmd" >&2; exit 1; }
done
for port in 18081 18082 18083 18084; do
  curl -s -m 5 -o /dev/null -X POST -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" -d "{}" "http://localhost:$port/mcp" || { echo "MCP servers not running; start them: ./bench/servers.sh up" >&2; exit 1; }
done
[ -n "$SCENARIOS" ] || { echo "no ground truth in $TRUTH_DIR; run: make demo-data" >&2; exit 1; }
mkdir -p "$OUT/configs"

for arm in $ARMS; do
  cp "bench/arms/$arm.json.tmpl" "$OUT/configs/$arm.json"
done

model_args=()
[ -n "$MODEL" ] && model_args=(--model "$MODEL")

for sc in $SCENARIOS; do
  truth="$TRUTH_DIR/$sc.truth.json"
  since=$(jq -r .since "$truth")
  until=$(jq -r .until "$truth")
  prompt=$(sed -e "s#{{SINCE}}#$since#g" -e "s#{{UNTIL}}#$until#g" bench/prompts/default.txt)
  for arm in $ARMS; do
    servers=$(jq -r '.mcpServers | keys | map("mcp__" + .) | join(",")' "$OUT/configs/$arm.json")
    for i in $(seq 1 "$RUNS"); do
      dest="$OUT/$sc.$arm.$i.json"
      if [ -s "$dest" ]; then echo "skip $dest (exists)"; continue; fi
      if [ -n "$TOTAL_USD" ]; then
        results=("$OUT"/*.*.*.json)
        spent=0
        [ -e "${results[0]}" ] && spent=$(jq -s 'map(.total_cost_usd // 0) | add // 0' "${results[@]}")
        if jq -en --argjson s "$spent" --argjson cap "$TOTAL_USD" '$s >= $cap' >/dev/null; then
          echo "stopping: spent \$$spent of the \$$TOTAL_USD cap" >&2; exit 3
        fi
      fi
      echo "run $sc arm=$arm #$i"
      # Each session runs isolated: a clean environment (so it cannot inherit
      # another Claude Code session's identity, context or cost tracking) and an
      # empty working directory (so file tools see nothing of this repo).
      work=$(mktemp -d)
      start=$(date +%s)
      ( cd "$work" && env -i HOME="$HOME" PATH="$PATH" ${ANTHROPIC_API_KEY:+ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY"} \
          claude -p "$prompt" \
          --mcp-config "$PWD_ABS/$OUT/configs/$arm.json" --strict-mcp-config \
          --restricted --allowedTools "$servers" \
          --output-format stream-json --verbose --max-budget-usd "$MAX_USD" "${model_args[@]}" \
        < /dev/null > "$PWD_ABS/$dest.stream" ) || echo "  (session exited non-zero; kept output)"
      rm -rf "$work"
      # Keep the final result plus the wall time and a count of tool calls by name.
      jq -s --argjson wall $(( $(date +%s) - start )) '
        ((map(select(.type == "result")) | last) // {is_error: true, result: ""})
        + {bench_wall_seconds: $wall,
           tool_calls: ([.[] | select(.type == "assistant") | .message.content[]? | select(.type == "tool_use") | .name]
                        | group_by(.) | map({key: .[0], value: length}) | from_entries)}' \
        "$dest.stream" > "$dest"
    done
  done
done
echo "done; grade with: python3 bench/grade.py $OUT $TRUTH_DIR"
