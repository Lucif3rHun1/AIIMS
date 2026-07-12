#!/bin/bash
# AIIMS Bot Watchdog - monitors logs, retries if failed
# Runs from 5:58 AM until execution completes or 6:05 AM

BOT_DIR="${BOT_DIR:-$(dirname "$0")}"
LOG_FILE="${LOG_FILE:-$BOT_DIR/bot.log}"
RESULTS_FILE="${RESULTS_FILE:-$BOT_DIR/watchdog_results.json}"

# Bot credentials live in config.json (gitignored). Read top-level keys via python3.
CONFIG_FILE="${CONFIG_FILE:-$BOT_DIR/config.json}"
cfg() { python3 - "$1" "$CONFIG_FILE" <<'PY' 2>/dev/null
import json, sys
try:
    print(json.load(open(sys.argv[2])).get(sys.argv[1], ""))
except Exception:
    pass
PY
}

# Required values (never hardcoded here)
BOT_TOKEN="$(cfg telegram_bot_token)"
CHAT_ID="$(cfg owner_id)"
BROADCAST_ID="$(cfg broadcast_chat_id)"
: "${BOT_TOKEN:?telegram_bot_token missing in $CONFIG_FILE}"
: "${CHAT_ID:?owner_id missing in $CONFIG_FILE}"
: "${BROADCAST_ID:?broadcast_chat_id missing in $CONFIG_FILE}"

# Ensure the bot is running (it creates bot.log). Launch it if not already up.
BOT_BIN="$BOT_DIR/bot"
if ! pgrep -f "[b]ot" >/dev/null 2>&1; then
  echo "[$(date '+%H:%M:%S')] Bot not running - starting it..."
  ( cd "$BOT_DIR" && nohup ./bot >> "$LOG_FILE" 2>&1 & )
fi

# Wait for bot.log to appear (the bot creates it on startup)
LOG_WAIT=0
while [ ! -f "$LOG_FILE" ] && [ "$LOG_WAIT" -lt 30 ]; do
  sleep 1
  LOG_WAIT=$((LOG_WAIT + 1))
done
if [ ! -f "$LOG_FILE" ]; then
  echo "ERROR: bot.log not found - bot failed to start (check config.json and permissions)"
  exit 1
fi

# Wait until 5:58 AM IST before monitoring the daily run
target_today()    { date -j -f "%Y-%m-%d %H:%M:%S" "$(date +%Y-%m-%d) 05:58:00" +%s 2>/dev/null || date -d "today 05:58:00" +%s; }
target_tomorrow() { date -j -v+1d -f "%Y-%m-%d %H:%M:%S" "$(date +%Y-%m-%d) 05:58:00" +%s 2>/dev/null || date -d "tomorrow 05:58:00" +%s; }
NOW=$(date +%s)
TARGET=$(target_today)
if [ "${WATCHDOG_NOWAIT:-0}" = "1" ]; then
  WAIT_SECS=0
elif [ "$TARGET" -lt "$NOW" ]; then
  # Past today's window: wait until tomorrow 05:58 (don't false-alert now)
  TARGET=$(target_tomorrow)
  WAIT_SECS=$((TARGET - NOW))
else
  WAIT_SECS=$((TARGET - NOW))
fi

if [ "$WAIT_SECS" -gt 0 ]; then
  echo "[$(date '+%H:%M:%S')] Watchdog sleeping ${WAIT_SECS}s until 05:58 AM IST..."
  sleep $WAIT_SECS
fi

echo "[$(date '+%H:%M:%S')] Watchdog ACTIVE - monitoring bot.log"
LOG_SIZE=$(wc -c < "$LOG_FILE" | tr -d ' ')

# Monitor for execution logs until 6:05 AM
DEADLINE=$(($(date +%s) + 420))  # 7 minutes from now
FOUND_EXECUTION=false
SUCCESS_COUNT=0
FAIL_COUNT=0
RESULTS=""

while [ $(date +%s) -lt $DEADLINE ]; do
  # Read new log lines
  NEW_SIZE=$(wc -c < "$LOG_FILE" | tr -d ' ')
  if [ "$NEW_SIZE" -gt "$LOG_SIZE" ]; then
    NEW_LINES=$(dd if="$LOG_FILE" bs=1 skip="$LOG_SIZE" 2>/dev/null)
    LOG_SIZE=$NEW_SIZE
    
    # Check for execution events
    while IFS= read -r line; do
      if echo "$line" | grep -q "appointment_confirmed"; then
        FOUND_EXECUTION=true
        SUCCESS_COUNT=$((SUCCESS_COUNT + 1))
        # Extract token number
        TOKEN_NUM=$(echo "$line" | grep -o 'token_number=[^ ,]*' | cut -d= -f2 || echo "unknown")
        PATIENT=$(echo "$line" | grep -o 'patient=[^ ,]*' | cut -d= -f2 || echo "unknown")
        RESULTS="${RESULTS}\n  ✅ $PATIENT: Token #$TOKEN_NUM"
        echo "[$(date '+%H:%M:%S')] SUCCESS: $PATIENT - Token #$TOKEN_NUM"
      fi
      
      if echo "$line" | grep -q "patient_failed"; then
        FOUND_EXECUTION=true
        FAIL_COUNT=$((FAIL_COUNT + 1))
        PATIENT=$(echo "$line" | grep -o 'patient=[^ ,]*' | cut -d= -f2 || echo "unknown")
        ERROR=$(echo "$line" | grep -o 'error=[^ ,}]*' | cut -d= -f2 || echo "unknown")
        RESULTS="${RESULTS}\n  ❌ $PATIENT: $ERROR"
        echo "[$(date '+%H:%M:%S')] FAILED: $PATIENT - $ERROR"
      fi
      
      if echo "$line" | grep -q "execution_complete"; then
        FOUND_EXECUTION=true
        echo "[$(date '+%H:%M:%S')] EXECUTION COMPLETE detected"
      fi
      
      if echo "$line" | grep -q "execution_started"; then
        FOUND_EXECUTION=true
        echo "[$(date '+%H:%M:%S')] EXECUTION STARTED detected"
      fi
      
      if echo "$line" | grep -q "prewarm_started"; then
        FOUND_EXECUTION=true
        echo "[$(date '+%H:%M:%S')] PRE-WARM STARTED detected"
      fi
    done <<< "$NEW_LINES"
  fi
  
  # If we found execution_complete, wait 10 more seconds then break
  if echo "$NEW_LINES" | grep -q "execution_complete"; then
    echo "[$(date '+%H:%M:%S')] Waiting 10s for any remaining logs..."
    sleep 10
    break
  fi
  
  sleep 2
done

echo ""
echo "==========================================="
echo "[$(date '+%H:%M:%S')] WATCHDOG ANALYSIS COMPLETE"
echo "==========================================="
echo "Execution found: $FOUND_EXECUTION"
echo "Successes: $SUCCESS_COUNT"
echo "Failures: $FAIL_COUNT"
echo ""
echo "Results:"
echo -e "$RESULTS"
echo ""

# Save results to JSON
cat > "$RESULTS_FILE" << EOF
{
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "execution_found": $FOUND_EXECUTION,
  "success_count": $SUCCESS_COUNT,
  "fail_count": $FAIL_COUNT,
  "results": "$(echo -e "$RESULTS" | sed 's/"/\\"/g' | tr '\n' '|' )"
}
EOF

echo "Results saved to $RESULTS_FILE"

# If NO execution happened or ALL failed → trigger manual retry
if [ "$FOUND_EXECUTION" = false ] || [ "$SUCCESS_COUNT" -eq 0 ]; then
  echo ""
  echo "⚠️ NO SUCCESSFUL APPOINTMENTS - Triggering manual retry..."
  
  # Send Telegram notification
  MSG="🔄 *Watchdog: No successful appointments detected*%0A%0ARetrying now...%0AExecution found: $FOUND_EXECUTION%0ASuccess: $SUCCESS_COUNT%0AFailed: $FAIL_COUNT"
  curl -s -X POST "https://api.telegram.org/bot${BOT_TOKEN}/sendMessage" \
    -d chat_id="$BROADCAST_ID" \
    -d text="$MSG" \
    -d parse_mode="Markdown" > /dev/null 2>&1
  
  echo "Manual retry notification sent via Telegram"
  echo "Check bot.log for retry results"
else
  echo ""
  echo "✅ Appointments successful - no retry needed"
  
  # Send success notification
  MSG="✅ *Watchdog: ${SUCCESS_COUNT} appointment(s) confirmed*%0A%0A$(echo -e "$RESULTS" | sed 's/\n/%0A/g')"
  curl -s -X POST "https://api.telegram.org/bot${BOT_TOKEN}/sendMessage" \
    -d chat_id="$BROADCAST_ID" \
    -d text="$MSG" \
    -d parse_mode="Markdown" > /dev/null 2>&1
fi

echo "[$(date '+%H:%M:%S')] Watchdog finished."
