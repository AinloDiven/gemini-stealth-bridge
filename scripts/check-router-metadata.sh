#!/bin/sh
# Validate that a router or gemini-stealth-bridge endpoint exposes full model metadata.
# Usage: scripts/check-router-metadata.sh <base-url> [api-key]

set -eu

if [ "$#" -lt 1 ]; then
  echo "Usage: $0 <base-url> [api-key]" >&2
  exit 2
fi

BASE_URL="${1%/}"
API_KEY="${2:-}"

MODELS_URL="$BASE_URL/v1/models"
case "$BASE_URL" in
  */v1) MODELS_URL="$BASE_URL/models" ;;
  */models) MODELS_URL="$BASE_URL" ;;
esac

TMP_JSON="$(mktemp)"
trap 'rm -f "$TMP_JSON"' EXIT

if [ -n "$API_KEY" ]; then
  curl -fsSL -H "Authorization: Bearer $API_KEY" "$MODELS_URL" -o "$TMP_JSON"
else
  curl -fsSL "$MODELS_URL" -o "$TMP_JSON"
fi

python3 - "$TMP_JSON" <<'PYEOF'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as f:
    payload = json.load(f)

data = payload.get("data", [])
if not isinstance(data, list) or not data:
    print("ERROR: /v1/models returned empty data array", file=sys.stderr)
    sys.exit(1)

mismatches = []
checked = 0

for m in data:
    mid = str(m.get("id", ""))
    if not (mid.startswith("gsb/") or "gemini" in mid.lower()):
        continue
    checked += 1
    ctx_len = m.get("context_length")
    max_comp = m.get("max_completion_tokens")
    max_out = m.get("max_output_tokens")
    reasoning = m.get("reasoning")
    supp_params = m.get("supported_parameters")
    caps = m.get("capabilities") or {}
    think_fmt = caps.get("thinkingFormat")

    if ctx_len in (128000, 32768, 4096) or not isinstance(ctx_len, int) or ctx_len < 1048576:
        mismatches.append(f"{mid}: context_length={ctx_len} (expected >= 1048576)")
    if max_comp in (4096, 8192, 32768) or not isinstance(max_comp, int) or max_comp < 65536:
        mismatches.append(f"{mid}: max_completion_tokens={max_comp} (expected >= 65536)")
    if max_out in (4096, 8192, 32768) or not isinstance(max_out, int) or max_out < 65536:
        mismatches.append(f"{mid}: max_output_tokens={max_out} (expected >= 65536)")
    if not isinstance(reasoning, bool):
        mismatches.append(f"{mid}: missing boolean 'reasoning' field")
    if not isinstance(supp_params, list) or "tools" not in supp_params:
        mismatches.append(f"{mid}: missing or incomplete 'supported_parameters'={supp_params}")
    if think_fmt != "gemini-level":
        mismatches.append(f"{mid}: capabilities.thinkingFormat={think_fmt!r} (expected 'gemini-level')")

if checked == 0:
    print("ERROR: no gsb/* or gemini-* models found in /v1/models", file=sys.stderr)
    sys.exit(1)

if mismatches:
    print(f"FAILED: {len(mismatches)} metadata mismatch(es) across {checked} model(s):", file=sys.stderr)
    for err in mismatches:
        print(f"  - {err}", file=sys.stderr)
    sys.exit(1)

print(f"OK: {checked} model(s) verified with full context_length, max_output_tokens, reasoning, and supported_parameters.")
PYEOF
