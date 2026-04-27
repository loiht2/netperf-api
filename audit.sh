#!/usr/bin/env bash
# audit.sh — Automated N×N bandwidth audit loop for netperf-api
#
# Prerequisites:
#   curl  (apt install curl  / brew install curl)
#   jq    (apt install jq   / brew install jq)
#
# Usage:
#   chmod +x audit.sh
#   ./audit.sh
#
# Quick test (2 iterations):
#   MAX_ITERATIONS=2 ./audit.sh
#
# Custom endpoint / log file:
#   API_URL=http://myhost:8080/api/v1/network-measure LOG_FILE=test.log ./audit.sh
#
# Background run:
#   nohup ./audit.sh > audit_console.log 2>&1 &
#   echo "PID: $!"

set -uo pipefail

# ── Configuration (override via environment) ─────────────────────────────────
API_URL="${API_URL:-http://localhost:8080/api/v1/network-measure}"
LOG_FILE="${LOG_FILE:-bandwidth_audit.log}"
MAX_ITERATIONS="${MAX_ITERATIONS:-200}"

# Max poll attempts per task (5 s each → 120 × 5 = 10 min timeout per iteration)
MAX_POLL_ATTEMPTS=120

# ── Logging helper ────────────────────────────────────────────────────────────
write_log_entry() {
    local iter="$1"
    local ts="$2"
    local task_id="$3"
    local matrix_json="$4"

    {
        printf "\n"
        printf "========== ITERATION %s | %s ==========\n" "$iter" "$ts"
        printf "Task ID: %s\n" "$task_id"
        printf "%s\n" "$matrix_json"
        printf "\n"
    } >> "$LOG_FILE"
}

# ── Startup banner ────────────────────────────────────────────────────────────
echo ""
echo "=============================================="
echo " netperf-api Bandwidth Audit"
echo " Iterations : $MAX_ITERATIONS"
echo " API URL    : $API_URL"
echo " Log file   : $LOG_FILE"
echo "=============================================="
echo ""

# ── Main loop ─────────────────────────────────────────────────────────────────
for i in $(seq 1 "$MAX_ITERATIONS"); do

    TIMESTAMP=$(date "+%Y-%m-%d %H:%M:%S")
    echo "---------- Iteration $i / $MAX_ITERATIONS | $TIMESTAMP ----------"

    # ── 1. Trigger measurement (POST) ────────────────────────────────────────
    POST_RESPONSE=$(curl -s --connect-timeout 3 --max-time 15 \
        -X POST "$API_URL" 2>/dev/null) || POST_RESPONSE=""

    if [ -z "$POST_RESPONSE" ]; then
        echo "  [WARN] No response from POST (API down?). Waiting 3 s then retrying loop..."
        sleep 3
        continue
    fi

    # jq's // empty returns "" instead of "null" on a missing key
    TASK_ID=$(printf '%s' "$POST_RESPONSE" | jq -r '.task_id // empty' 2>/dev/null) || TASK_ID=""

    if [ -z "$TASK_ID" ]; then
        echo "  [WARN] Could not parse task_id. Raw POST response:"
        echo "         $POST_RESPONSE"
        echo "         Waiting 3 s..."
        sleep 3
        continue
    fi

    echo "  Task ID : $TASK_ID"

    # ── 2. Poll until completed (or terminal state / timeout) ─────────────────
    STATUS=""
    GET_RESPONSE=""
    POLL_ATTEMPT=0

    while [ "$STATUS" != "completed" ]; do

        POLL_ATTEMPT=$((POLL_ATTEMPT + 1))

        if [ "$POLL_ATTEMPT" -gt "$MAX_POLL_ATTEMPTS" ]; then
            echo "  [WARN] Timed out after $MAX_POLL_ATTEMPTS polls (~$((MAX_POLL_ATTEMPTS * 5)) s). Giving up."
            STATUS="timeout"
            break
        fi

        sleep 5

        GET_RESPONSE=$(curl -s --connect-timeout 3 --max-time 15 \
            "${API_URL}/${TASK_ID}" 2>/dev/null) || GET_RESPONSE=""

        if [ -z "$GET_RESPONSE" ]; then
            echo "  [WARN] Empty GET response on poll $POLL_ATTEMPT. Retrying..."
            continue
        fi

        STATUS=$(printf '%s' "$GET_RESPONSE" | jq -r '.status // empty' 2>/dev/null) || STATUS=""
        echo "  [poll $POLL_ATTEMPT] status = ${STATUS:-unknown}"

        # Abort polling on any terminal non-completed state
        if [ "$STATUS" = "failed" ] || [ "$STATUS" = "canceled" ]; then
            echo "  [WARN] Task ended with status=$STATUS. Moving to next iteration."
            break
        fi

    done

    # ── 3. Extract matrix and write log entry ─────────────────────────────────
    if [ "$STATUS" = "completed" ] && [ -n "$GET_RESPONSE" ]; then

        MATRIX_JSON=$(printf '%s' "$GET_RESPONSE" \
            | jq '.result.matrix' 2>/dev/null) || MATRIX_JSON="null"

        write_log_entry "$i" "$TIMESTAMP" "$TASK_ID" "$MATRIX_JSON"
        echo "  [OK] Result logged to $LOG_FILE"

    else
        echo "  [SKIP] Iteration $i not logged (status=${STATUS:-unknown})."
    fi

    # ── 4. Cooldown before next iteration ─────────────────────────────────────
    echo "  Iteration $i complete. Sleeping 30 s..."
    echo ""
    sleep 30

done

echo ""
echo "=============================================="
echo " Audit complete: $MAX_ITERATIONS iteration(s)"
echo " Log: $LOG_FILE"
echo "=============================================="
