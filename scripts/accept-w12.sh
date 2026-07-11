#!/usr/bin/env bash
# scripts/accept-w12.sh -- the W1-2 LIVE acceptance gate (`make accept-w12`,
# W12-10). Companion to tests/e2e/w12_gate_test.go's HERMETIC gate: this
# script runs the same flow against the REAL kahyad daemon (launchd or
# `make run-daemon`, a real Anthropic credential/session wired per
# kahyad/internal/anthproxy's credential_mode) and the REAL ~/Kahya seed
# corpus (W0-01) -- no mock server, no fixture corpus.
#
# Prints one PASS / FAIL / DEFERRED line per criterion and a final summary.
# Exits nonzero if any criterion FAILs. A DEFERRED criterion means this
# environment could not complete a real model call (no live Anthropic
# credential/Claude Code session available) -- by itself that does not
# force a nonzero exit, since the point of DEFERRED is "not verified here",
# not "verified and broken". Re-run this script once a live credential is
# configured to turn every DEFERRED line into a real PASS/FAIL.
#
# See docs/ipc.md's "W1-2 gate -- how to re-run" appendix for the exact
# three commands this whole gate boils down to.

set -uo pipefail

DATA_DIR="${KAHYA_DATA_DIR:-$HOME/Library/Application Support/Kahya}"
SOCK="${KAHYA_SOCKET:-$DATA_DIR/kahyad.sock}"
DB_PATH="${KAHYA_DB_PATH:-$DATA_DIR/brain.db}"
LOG_DIR="$DATA_DIR/logs"
KAHYAD_LOG="$LOG_DIR/kahyad.jsonl"
WORKER_LOG="$LOG_DIR/worker.jsonl"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KAHYA_BIN="$REPO_ROOT/bin/kahya"

FAILS=0
DEFERRED=0

pass()     { printf 'PASS      %s\n' "$1"; }
fail()     { printf 'FAIL      %s -- %s\n' "$1" "$2"; FAILS=$((FAILS+1)); }
deferred() { printf 'DEFERRED  %s -- %s\n' "$1" "$2"; DEFERRED=$((DEFERRED+1)); }

echo "=== W1-2 LIVE acceptance gate (make accept-w12) ==="
echo "socket:  $SOCK"
echo "db:      $DB_PATH"
echo "logs:    $LOG_DIR"
echo

if [ ! -x "$KAHYA_BIN" ]; then
	fail "health" "$KAHYA_BIN not found/executable -- run 'make build' first"
	echo
	echo "=== SUMMARY: $FAILS FAIL, $DEFERRED DEFERRED ==="
	exit 1
fi

# --- 1. health ---
HEALTH_OUT="$("$KAHYA_BIN" health 2>&1)"
HEALTH_STATUS=$?
if [ "$HEALTH_STATUS" -eq 0 ]; then
	pass "health ($HEALTH_OUT)"
else
	fail "health" "kahya health exited $HEALTH_STATUS: $HEALTH_OUT"
	echo
	echo "kahyad is not reachable at $SOCK -- start it (launchd 'make install-agent'"
	echo "or 'make run-daemon' in another terminal) and re-run. Every remaining"
	echo "criterion is DEFERRED."
	deferred "evlerimizden-search" "daemon unreachable"
	deferred "answer"               "daemon unreachable"
	deferred "trace"                "daemon unreachable"
	deferred "ledger-forensics"      "daemon unreachable"
	echo
	echo "=== SUMMARY: $FAILS FAIL, $DEFERRED DEFERRED ==="
	exit 1
fi

# --- 2. 'evlerimizden' search against the REAL corpus ---
SEARCH_JSON="$(curl -sS --unix-socket "$SOCK" -X POST http://kahyad/v1/memory/search \
	-H 'Content-Type: application/json' \
	-d '{"query":"evlerimizden","k":3}' 2>&1)"
if [ $? -ne 0 ]; then
	fail "evlerimizden-search" "curl failed: $SEARCH_JSON"
else
	FOUND="$(python3 - "$SEARCH_JSON" <<'PYEOF'
import json, sys
try:
    data = json.loads(sys.argv[1])
except Exception as e:
    print("PARSE_ERROR:" + str(e))
    sys.exit(0)
results = data.get("results", [])[:3]
hit = any("ev" in (r.get("text", "").lower()) for r in results)
print("FOUND" if hit else "NOT_FOUND")
PYEOF
	)"
	case "$FOUND" in
	FOUND)
		pass "evlerimizden-search (top-3 contains a seed note with substring 'ev')" ;;
	NOT_FOUND)
		fail "evlerimizden-search" "top-3 results contain no note with substring 'ev' -- if the real ~/Kahya corpus genuinely has none, this is blocked-user (HANDOFF §6 expects the seeded iOS home-design note, 'iOS ev tasarım app'): confirm one exists with the user, do NOT weaken this criterion. Response: $SEARCH_JSON" ;;
	*)
		fail "evlerimizden-search" "could not parse response: $SEARCH_JSON" ;;
	esac
fi

# --- 3. the live model-call flow (needs a real Anthropic credential/session) ---
echo
echo "Attempting the live task call: bin/kahya \"Evlerimizden ne konuşmuştuk?\" ..."
TASK_OUT_FILE="$(mktemp)"
TASK_ERR_FILE="$(mktemp)"
trap 'rm -f "$TASK_OUT_FILE" "$TASK_ERR_FILE"' EXIT

"$KAHYA_BIN" "Evlerimizden ne konuşmuştuk?" >"$TASK_OUT_FILE" 2>"$TASK_ERR_FILE"
TASK_STATUS=$?
TASK_STDOUT="$(cat "$TASK_OUT_FILE")"
TASK_STDERR="$(cat "$TASK_ERR_FILE")"
TRACE_ID="$(printf '%s\n' "$TASK_STDERR" | sed -n 's/^iz: //p' | head -n1)"

if [ "$TASK_STATUS" -ne 0 ] || [ -z "$TASK_STDOUT" ]; then
	echo "--- stdout ---"
	echo "$TASK_STDOUT"
	echo "--- stderr ---"
	echo "$TASK_STDERR"
	echo
	echo "No successful live answer (exit=$TASK_STATUS). This is expected on a"
	echo "machine with no real Anthropic credential/Claude Code session wired up"
	echo "yet -- deferring the criteria that depend on it rather than failing them."
	deferred "answer"          "no real Anthropic credential/Claude Code session available (task exited $TASK_STATUS) -- run this script by hand once one is configured"
	deferred "trace"           "depends on a successful live task call above"
	deferred "ledger-forensics" "depends on a successful live task call above"
else
	pass "answer (non-empty stdout, trace_id=$TRACE_ID)"

	# --- trace: every kahyad.jsonl/worker.jsonl line containing TRACE_ID
	# must parse as JSON with .trace_id == TRACE_ID, and each file must
	# have at least one such line. ---
	trace_check() {
		python3 - "$1" "$TRACE_ID" <<'PYEOF'
import json, sys
path, trace_id = sys.argv[1], sys.argv[2]
matches = bad = 0
try:
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or trace_id not in line:
                continue
            matches += 1
            try:
                obj = json.loads(line)
            except Exception:
                bad += 1
                continue
            if obj.get("trace_id") != trace_id:
                bad += 1
except FileNotFoundError:
    print("0 -1")
    sys.exit(0)
print(matches, bad)
PYEOF
	}
	read -r KAHYAD_MATCHES KAHYAD_BAD <<<"$(trace_check "$KAHYAD_LOG")"
	read -r WORKER_MATCHES WORKER_BAD <<<"$(trace_check "$WORKER_LOG")"

	if [ "${KAHYAD_BAD:-0}" -eq -1 ] 2>/dev/null || [ "${WORKER_BAD:-0}" -eq -1 ] 2>/dev/null; then
		fail "trace" "kahyad.jsonl or worker.jsonl not found under $LOG_DIR"
	elif [ "${KAHYAD_MATCHES:-0}" -gt 0 ] && [ "${KAHYAD_BAD:-0}" -eq 0 ] && \
	     [ "${WORKER_MATCHES:-0}" -gt 0 ] && [ "${WORKER_BAD:-0}" -eq 0 ]; then
		pass "trace (single trace_id=$TRACE_ID across kahyad.jsonl + worker.jsonl)"
	else
		fail "trace" "kahyad.jsonl matches=$KAHYAD_MATCHES bad=$KAHYAD_BAD, worker.jsonl matches=$WORKER_MATCHES bad=$WORKER_BAD"
	fi

	# --- ledger forensics: hafiza_injected.block sha256 == its own
	# block_sha256 field (HANDOFF §5 safety #4 self-consistency; comparing
	# against the actual outbound Anthropic request body -- as the
	# hermetic gate does against its mock -- is not reproducible here
	# without also intercepting real Anthropic traffic). ---
	PAYLOAD_JSON="$(sqlite3 "$DB_PATH" "SELECT payload FROM events WHERE trace_id='$TRACE_ID' AND kind='hafiza_injected' LIMIT 1;" 2>&1)"
	if [ -z "$PAYLOAD_JSON" ]; then
		fail "ledger-forensics" "no hafiza_injected event for trace_id=$TRACE_ID in $DB_PATH"
	else
		LEDGER_CHECK="$(python3 - "$PAYLOAD_JSON" <<'PYEOF'
import json, sys, hashlib
try:
    payload = json.loads(sys.argv[1])
except Exception as e:
    print("PARSE_ERROR", str(e))
    sys.exit(0)
block = payload.get("block", "")
want = payload.get("block_sha256", "")
got = hashlib.sha256(block.encode("utf-8")).hexdigest()
print(got, want)
PYEOF
		)"
		GOT_SHA="$(awk '{print $1}' <<<"$LEDGER_CHECK")"
		WANT_SHA="$(awk '{print $2}' <<<"$LEDGER_CHECK")"
		if [ "$GOT_SHA" = "PARSE_ERROR" ]; then
			fail "ledger-forensics" "could not parse hafiza_injected payload JSON: $PAYLOAD_JSON"
		elif [ -n "$WANT_SHA" ] && [ "$GOT_SHA" = "$WANT_SHA" ]; then
			pass "ledger-forensics (sha256(hafiza_injected.block) == hafiza_injected.block_sha256)"
		else
			fail "ledger-forensics" "sha256(block)=$GOT_SHA, block_sha256=$WANT_SHA"
		fi
	fi
fi

echo
echo "=== SUMMARY: $FAILS FAIL, $DEFERRED DEFERRED ==="
if [ "$FAILS" -gt 0 ]; then
	exit 1
fi
exit 0
