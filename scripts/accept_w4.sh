#!/usr/bin/env bash
# scripts/accept_w4.sh -- the W4-07 durability acceptance gate (`make
# accept-w4`, HANDOFF §6 W4). Companion to tests/acceptance/w4/
# w4_gate_test.go's CI-speed Go harness: this script drives THREE
# real-daemon (KAHYA_ENV=dev) runs of scenarios A/B/C using the compiled
# bin/kahyad + bin/kahya + bin/kahya-mcp and the same scripted fixture
# workers/policy overlay the Go test uses, so it exercises the identical
# code paths through the actual CLI a person would type.
#
# Cloud-free and headless by design: no real Anthropic session/API key,
# Keychain item, or network access is ever needed -
#   - scenario A's stub tool (w2_slow_stub) is a pure local MCP call
#   - scenario B's "cloud" upstream is a python fixture worker hitting
#     kahyad's own per-task forward-proxy, pointed at a port nothing (then
#     later a local fake responder) listens on
#   - scenario C's anchor remote is a local `git init --bare` repo (file://)
#     and KAHYA_ANCHOR_KEY_OVERRIDE (dev-only) stands in for a real
#     kahya.anchor Keychain deploy key
#
# HARD CONSTRAINT (W4-07 task spec): nothing here may open/modify the real
# ~/Library/Application Support/Kahya/brain.db or ~/Kahya. Every daemon
# this script starts runs under KAHYA_ENV=dev AND fresh scratch
# directories (mktemp) - never the real ~/Kahya-dev either, so repeated
# runs are hermetic and parallel-safe. The prod brain.db's own
# MAX(events.id) is recorded before and after the ENTIRE run and compared -
# any difference fails the gate outright, independent of anything else.
#
# Duration knobs:
#   W4_REAL=1   switches scenario A's stub duration to >=600s (+ filler
#               steps) for the real-time evidence run tasks/w4-durability/
#               W4-07-w4-acceptance.md's own acceptance criteria require
#               once; appends `accept.w4_real_run` to the dev ledger with
#               {duration_s, scenario_results}. Absent (default): fast mode,
#               whole script ~90s.
#
# Exit: nonzero if ANY scenario FAILs, or if the prod-brain.db-untouched
# check fails.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KAHYAD_BIN="$REPO_ROOT/bin/kahyad"
KAHYA_BIN="$REPO_ROOT/bin/kahya"
FIXTURES_DIR="$REPO_ROOT/tests/acceptance/w4/fixtures"

W4_REAL="${W4_REAL:-0}"
if [ "$W4_REAL" = "1" ]; then
	SCENARIO_A_DURATION_S=600
	SCENARIO_A_TIMEOUT_S=900
else
	SCENARIO_A_DURATION_S=3
	SCENARIO_A_TIMEOUT_S=30
fi

PYTHON3="$(command -v python3 || true)"
if [ -z "$PYTHON3" ]; then
	echo "python3 not found on PATH -- required by the fixture workers" >&2
	exit 2
fi

for bin in "$KAHYAD_BIN" "$KAHYA_BIN"; do
	if [ ! -x "$bin" ]; then
		echo "$bin not found/executable -- run 'make build' first" >&2
		exit 2
	fi
done

RUN_LOG_DIR="$HOME/Library/Logs/Kahya"
mkdir -p "$RUN_LOG_DIR"
RUN_LOG="$RUN_LOG_DIR/accept-w4-$(date +%Y%m%d-%H%M%S).log"
echo "=== W4-07 durability acceptance gate (make accept-w4) — $(date -u +%Y-%m-%dT%H:%M:%SZ) ===" | tee "$RUN_LOG"
echo "W4_REAL=$W4_REAL  scenario A duration=${SCENARIO_A_DURATION_S}s  run log=$RUN_LOG" | tee -a "$RUN_LOG"

log() { echo "$@" | tee -a "$RUN_LOG"; }

# NOTE: this script targets bash 3.2 (macOS's own /bin/bash - no Homebrew
# bash dependency) deliberately, so it never uses associative arrays
# (declare -A, bash 4+ only) or other post-3.2 bashisms - scenario results
# are plain named variables (RESULT_A/RESULT_B/RESULT_C), not a map.
FAILS=0
RESULT_A=""
RESULT_B=""
RESULT_C=""
pass() { log "PASS      $1"; }
fail() { log "FAIL      $1 -- $2"; FAILS=$((FAILS+1)); }

# --- PROD SAFETY (task spec's own acceptance criterion) ---
PROD_DB="$HOME/Library/Application Support/Kahya/brain.db"
prod_max_event_id() {
	if [ -f "$PROD_DB" ]; then
		sqlite3 "$PROD_DB" "SELECT COALESCE(MAX(id),0) FROM events;" 2>/dev/null || echo "0"
	else
		echo "0"
	fi
}
PROD_MAX_BEFORE="$(prod_max_event_id)"
log "prod brain.db MAX(events.id) before run: $PROD_MAX_BEFORE (db: $PROD_DB)"

# --- scratch base (HARD CONSTRAINT: temp dirs only, never the real
# ~/Kahya-dev either) ---
SCRATCH_BASE="$(mktemp -d "${TMPDIR:-/tmp}/kahya-accept-w4.XXXXXX")"
log "scratch base: $SCRATCH_BASE"

RUNNING_PIDS=()
cleanup() {
	for pid in "${RUNNING_PIDS[@]:-}"; do
		[ -n "$pid" ] && kill -0 "$pid" >/dev/null 2>&1 && kill -TERM "$pid" >/dev/null 2>&1
	done
	sleep 0.3
	for pid in "${RUNNING_PIDS[@]:-}"; do
		[ -n "$pid" ] && kill -0 "$pid" >/dev/null 2>&1 && kill -KILL "$pid" >/dev/null 2>&1
	done
	rm -rf "$SCRATCH_BASE"
}
trap cleanup EXIT

# free_port prints a currently-free 127.0.0.1 TCP port (bind, read, release
# - the standard, if TOCTOU-imperfect, idiom this whole codebase's own Go
# tests already use).
free_port() {
	"$PYTHON3" -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

new_trace_id() { "$PYTHON3" -c 'import secrets; print(secrets.token_hex(16))'; }

json_get() {
	# json_get <json-string> <dotted.path> -- tiny helper, python3-backed
	# (no jq dependency).
	"$PYTHON3" -c '
import json, sys
d = json.loads(sys.argv[1])
path = sys.argv[2].split(".")
for p in path:
    if d is None:
        break
    d = d.get(p) if isinstance(d, dict) else None
print(d if d is not None else "")
' "$1" "$2"
}

# --- daemon lifecycle ---
# prepare_daemon <name> <config_yaml_body> -- lays out a fresh scratch
# directory tree + config.yaml + a COPY of the fixture policy.yaml (a
# caller that needs to patch the policy - scenario B's loopback allowlist
# entry - does so against D_POLICY between calling this and launch_daemon).
# Sets globals: D_HOME D_DATA D_MEM D_SOCK D_DB D_POLICY D_OUT D_ERR
prepare_daemon() {
	local name="$1" config_body="$2"
	local root="$SCRATCH_BASE/$name"
	D_HOME="$root/home"; D_MEM="$root/home/memory"
	# Runtime DATA (brain.db) lives under the dev-profile Kahya-dev dir (a
	# non-prod path, so config.Load's refuseDevProfileOpeningProdDB is
	# satisfied); config.yaml, however, has ONE canonical env-independent
	# location - the prod (HOME-derived) Kahya dir - which is the ONLY place
	# config.Load ever reads it from, even under KAHYA_ENV=dev (see config.go
	# Load's own doc comment). Writing it under Kahya-dev would make it a
	# silent no-op.
	D_DATA="$root/home/Library/Application Support/Kahya-dev"
	D_CONFIG="$root/home/Library/Application Support/Kahya"
	D_SOCK="$root/k.sock"; D_DB="$D_DATA/brain.db"
	D_POLICY="$root/policy.yaml"
	D_OUT="$root/kahyad.stdout.log"; D_ERR="$root/kahyad.stderr.log"
	mkdir -p "$D_MEM" "$D_DATA" "$D_CONFIG"
	(cd "$D_MEM" && git init -q && git config user.email "kahya-accept-w4@example.invalid" && git config user.name "Kahya accept-w4")
	printf '%s\n' "$config_body" > "$D_CONFIG/config.yaml"
	cp "$FIXTURES_DIR/policy.yaml" "$D_POLICY"
}

# launch_daemon <name> [extra_env_KEY=VAL ...] -- execs kahyad against the
# CURRENT D_HOME/D_DATA/D_MEM/D_SOCK/D_POLICY (prepare_daemon's globals,
# possibly patched in between) and waits for /health. Sets D_PID. Safe to
# call more than once against the same D_* globals (a restart).
launch_daemon() {
	local name="$1"; shift
	env -i \
		HOME="$D_HOME" PATH="$PATH" \
		KAHYA_ENV=dev \
		KAHYA_DATA_DIR="$D_DATA" KAHYA_MEMORY_DIR="$D_MEM" KAHYA_SOCKET="$D_SOCK" \
		KAHYA_POLICY_PATH="$D_POLICY" KAHYA_LOG_LEVEL=info \
		KAHYA_ANCHOR_KEY_OVERRIDE=accept-w4-dev-placeholder-key \
		PYTHONUTF8=1 \
		"$@" \
		"$KAHYAD_BIN" >"$D_OUT" 2>"$D_ERR" &
	D_PID=$!
	RUNNING_PIDS+=("$D_PID")

	local deadline=$((SECONDS+20))
	while [ $SECONDS -lt $deadline ]; do
		if [ -S "$D_SOCK" ] && curl -s --unix-socket "$D_SOCK" -o /dev/null -w '%{http_code}' http://x/health 2>/dev/null | grep -q '^200$'; then
			return 0
		fi
		sleep 0.2
	done
	log "kahyad ($name) never became healthy; stderr tail:"
	tail -n 30 "$D_ERR" | tee -a "$RUN_LOG"
	return 1
}

# start_daemon <name> <config_yaml_body> [extra_env_KEY=VAL ...] --
# prepare_daemon + launch_daemon in one call (scenario A's own, single-boot
# use case).
start_daemon() {
	local name="$1" config_body="$2"; shift 2
	prepare_daemon "$name" "$config_body"
	launch_daemon "$name" "$@"
}

stop_daemon() {
	local pid="$1"
	kill -INT "$pid" >/dev/null 2>&1
	local deadline=$((SECONDS+8))
	while kill -0 "$pid" >/dev/null 2>&1 && [ $SECONDS -lt $deadline ]; do sleep 0.2; done
	kill -KILL "$pid" >/dev/null 2>&1
	wait "$pid" 2>/dev/null
}

cli() {
	# cli <D_SOCK> <D_HOME> <args...>
	local sock="$1" home="$2"; shift 2
	env -i HOME="$home" PATH="$PATH" KAHYA_SOCKET="$sock" "$KAHYA_BIN" "$@"
}

promote_to_auto_allow() {
	local sock="$1" home="$2" tool="$3" class="$4" scope="$5" n="$6" i
	for i in $(seq 1 "$n"); do
		cli "$sock" "$home" autonomy promote "$tool" "$class" "$scope" >/dev/null
	done
}

post_task() {
	# post_task <sock> <trace_id> <prompt> -- fires the SSE request in the
	# background (discarding its body - this script cares about ledger/db
	# side effects, not the streamed text) and returns immediately.
	local sock="$1" trace="$2" prompt="$3"
	( curl -s --unix-socket "$sock" -X POST http://x/v1/task \
		-d "$(printf '{"trace_id":%s,"prompt":%s}' "$(json_quote "$trace")" "$(json_quote "$prompt")")" \
		>/dev/null 2>&1 & )
}

json_quote() { "$PYTHON3" -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$1"; }

wait_for_task_id() {
	# wait_for_task_id <db> <trace_id> <timeout_s> -- prints the task_id.
	local db="$1" trace="$2" timeout="$3"
	local deadline=$((SECONDS+timeout))
	while [ $SECONDS -lt $deadline ]; do
		local payload
		payload="$(sqlite3 "$db" "SELECT payload FROM events WHERE trace_id='$trace' AND kind='task_spawned' ORDER BY id LIMIT 1;" 2>/dev/null || true)"
		if [ -n "$payload" ]; then
			json_get "$payload" "task_id"
			return 0
		fi
		sleep 0.2
	done
	return 1
}

wait_for_tool_call_status() {
	# wait_for_tool_call_status <db> <task_id> <tool> <status> <timeout_s>
	local db="$1" task_id="$2" tool="$3" want="$4" timeout="$5"
	local deadline=$((SECONDS+timeout))
	while [ $SECONDS -lt $deadline ]; do
		local got
		got="$(sqlite3 "$db" "SELECT status FROM tool_calls WHERE task_id='$task_id' AND tool_name='$tool' ORDER BY id DESC LIMIT 1;" 2>/dev/null || true)"
		[ "$got" = "$want" ] && return 0
		sleep 0.1
	done
	return 1
}

wait_for_task_status() {
	# wait_for_task_status <db> <task_id> <timeout_s> <want1> [want2 ...]
	local db="$1" task_id="$2" timeout="$3"; shift 3
	local deadline=$((SECONDS+timeout))
	while [ $SECONDS -lt $deadline ]; do
		local got
		got="$(sqlite3 "$db" "SELECT status FROM tasks WHERE id='$task_id';" 2>/dev/null || true)"
		for want in "$@"; do
			[ "$got" = "$want" ] && { echo "$got"; return 0; }
		done
		sleep 0.2
	done
	sqlite3 "$db" "SELECT status FROM tasks WHERE id='$task_id';" 2>/dev/null || echo ""
	return 1
}

count_events() {
	local db="$1" trace="$2" kind="$3"
	sqlite3 "$db" "SELECT COUNT(*) FROM events WHERE trace_id='$trace' AND kind='$kind';" 2>/dev/null || echo "0"
}

# =========================================================================
# Scenario A: kill-resume, no double execution
# =========================================================================
run_scenario_a() {
	local counter_file="$SCRATCH_BASE/scenario-a-counter.txt"
	local pid_file="$SCRATCH_BASE/scenario-a-worker.pid"
	local duration_ms=$((SCENARIO_A_DURATION_S * 1000))

	if ! start_daemon scenario-a "$(cat <<EOF
task_timeout_min: $(( (SCENARIO_A_TIMEOUT_S/60) + 2 ))
embed_cmd: []
egress_port: $(free_port)
worker_cmd: ["$PYTHON3", "$FIXTURES_DIR/w2_worker.py"]
resume_scan_interval_seconds: 1
outbox_dispatch_interval_seconds: 1
EOF
)" \
		KAHYA_W2_STUB_DURATION_MS="$duration_ms" \
		KAHYA_W2_STUB_COUNTER_FILE="$counter_file" \
		KAHYA_W2_STUB_PID_FILE="$pid_file"
	then
		RESULT_A=FAIL; fail "A: kill-resume no-double-execution" "daemon never became healthy"
		return
	fi
	local daemon_pid=$D_PID sock=$D_SOCK home=$D_HOME db=$D_DB

	promote_to_auto_allow "$sock" "$home" w2_slow_stub W2 global 3

	local trace; trace="$(new_trace_id)"
	post_task "$sock" "$trace" "accept_w4 scenario A probe"

	local task_id
	if ! task_id="$(wait_for_task_id "$db" "$trace" 10)"; then
		RESULT_A=FAIL; fail "A: kill-resume no-double-execution" "no task_spawned event within 10s"
		stop_daemon "$daemon_pid"; return
	fi
	log "A: task_id=$task_id trace=$trace"

	if ! wait_for_tool_call_status "$db" "$task_id" w2_slow_stub executing 10; then
		RESULT_A=FAIL; fail "A: kill-resume no-double-execution" "tool_calls row never reached status=executing"
		stop_daemon "$daemon_pid"; return
	fi

	local deadline=$((SECONDS+5)) pid=""
	while [ $SECONDS -lt $deadline ]; do
		[ -s "$pid_file" ] && { pid="$(cat "$pid_file")"; break; }
		sleep 0.1
	done
	if [ -z "$pid" ]; then
		RESULT_A=FAIL; fail "A: kill-resume no-double-execution" "worker pid file never appeared"
		stop_daemon "$daemon_pid"; return
	fi
	log "A: killing worker pid=$pid mid-tool-call (\`kahya task show $task_id\` would report this same pid)"
	kill -KILL "$pid" 2>/dev/null

	local final_status
	if ! final_status="$(wait_for_task_status "$db" "$task_id" "$SCENARIO_A_TIMEOUT_S" done failed blocked_user)"; then
		RESULT_A=FAIL; fail "A: kill-resume no-double-execution" "task never reached a terminal status within ${SCENARIO_A_TIMEOUT_S}s (last=$final_status)"
		stop_daemon "$daemon_pid"; return
	fi
	if [ "$final_status" != "done" ]; then
		RESULT_A=FAIL; fail "A: kill-resume no-double-execution" "task ended in status=$final_status, want done"
		stop_daemon "$daemon_pid"; return
	fi

	sleep 0.5
	local lines=0
	[ -f "$counter_file" ] && lines="$(wc -l < "$counter_file" | tr -d ' ')"
	local receipt_count
	receipt_count="$(sqlite3 "$db" "SELECT COUNT(*) FROM tool_calls WHERE task_id='$task_id' AND tool_name='w2_slow_stub' AND status='receipt';")"
	local replayed_count
	replayed_count="$(count_events "$db" "$trace" tool.replayed)"

	log "A: wc -l counter_file = $lines (want 1); receipt-count = $receipt_count (want 1); tool.replayed events = $replayed_count (want >=1)"

	if [ "$lines" = "1" ] && [ "$receipt_count" = "1" ] && [ "$replayed_count" -ge 1 ]; then
		RESULT_A=PASS; pass "A: kill-resume no-double-execution"
	else
		RESULT_A=FAIL; fail "A: kill-resume no-double-execution" "evidence mismatch (see counts above)"
	fi

	stop_daemon "$daemon_pid"
}

# =========================================================================
# Scenario B: offline -> reconnect
# =========================================================================
run_scenario_b() {
	local port; port="$(free_port)"
	local upstream="http://127.0.0.1:$port"

	prepare_daemon scenario-b "$(cat <<EOF
task_timeout_min: 2
embed_cmd: []
egress_port: $(free_port)
worker_cmd: ["$PYTHON3", "$FIXTURES_DIR/cloud_worker.py"]
anthropic_upstream_url: "$upstream"
cloud_retry_max_inline: 1
cloud_retry_task_schedule: ["1s"]
cloud_retry_give_up_after: "60s"
resume_scan_interval_seconds: 1
outbox_dispatch_interval_seconds: 1
EOF
)"
	# Patch the fixture policy's egress allowlist for the loopback upstream
	# port (kahyad/internal/egress.Gate denies loopback unless explicitly
	# allowlisted) BEFORE ever launching the daemon.
	"$PYTHON3" - "$D_POLICY" "$port" <<'PYEOF'
import sys
path, port = sys.argv[1], sys.argv[2]
with open(path) as f:
    content = f.read()
anchor = "egress:\n  allowlist:\n"
patch = "egress:\n  allowlist:\n    - host: 127.0.0.1\n      ports: [%s]\n" % port
if anchor not in content:
    raise SystemExit("anchor not found in policy.yaml")
content = content.replace(anchor, patch, 1)
with open(path, "w") as f:
    f.write(content)
PYEOF

	if ! launch_daemon scenario-b; then
		RESULT_B=FAIL; fail "B: offline -> reconnect completes" "daemon never became healthy"
		return
	fi
	local daemon_pid=$D_PID sock=$D_SOCK home=$D_HOME db=$D_DB

	local trace; trace="$(new_trace_id)"
	post_task "$sock" "$trace" "accept_w4 scenario B probe"

	local task_id
	if ! task_id="$(wait_for_task_id "$db" "$trace" 10)"; then
		RESULT_B=FAIL; fail "B: offline -> reconnect completes" "no task_spawned event within 10s"
		stop_daemon "$daemon_pid"; return
	fi
	log "B: task_id=$task_id trace=$trace (blackhole upstream port=$port)"

	if ! wait_for_task_status "$db" "$task_id" 15 bekliyor-yeniden-deneme >/dev/null; then
		RESULT_B=FAIL; fail "B: offline -> reconnect completes" "task never reached bekliyor-yeniden-deneme"
		stop_daemon "$daemon_pid"; return
	fi
	local parked_count; parked_count="$(count_events "$db" "$trace" task.waiting_retry)"
	if [ "$parked_count" -lt 1 ]; then
		RESULT_B=FAIL; fail "B: offline -> reconnect completes" "no task.waiting_retry event"
		stop_daemon "$daemon_pid"; return
	fi
	log "B: parked in bekliyor-yeniden-deneme (task.waiting_retry events=$parked_count)"

	# Network back: bind a real, healthy fake responder to the SAME port.
	"$PYTHON3" "$FIXTURES_DIR/fake_anthropic_upstream.py" "$port" >"$SCRATCH_BASE/fake-upstream.log" 2>&1 &
	local fake_pid=$!
	RUNNING_PIDS+=("$fake_pid")
	disown "$fake_pid" 2>/dev/null # suppress bash's own job-control "Terminated" notice when this is killed below
	sleep 0.3

	local final_status
	if ! final_status="$(wait_for_task_status "$db" "$task_id" 15 done failed)"; then
		RESULT_B=FAIL; fail "B: offline -> reconnect completes" "task never reached done/failed after reconnect"
	elif [ "$final_status" != "done" ]; then
		RESULT_B=FAIL; fail "B: offline -> reconnect completes" "task ended in status=$final_status after reconnect, want done"
	else
		RESULT_B=PASS; pass "B: offline -> reconnect completes"
	fi

	kill -TERM "$fake_pid" 2>/dev/null
	stop_daemon "$daemon_pid"
}

# =========================================================================
# Scenario C: local ledger tamper vs remote anchor
# =========================================================================
run_scenario_c() {
	local bare_remote="$SCRATCH_BASE/anchor-remote.git"
	git init --bare -q "$bare_remote"

	local cfg
	cfg="$(cat <<EOF
task_timeout_min: 2
embed_cmd: []
egress_port: $(free_port)
anchor_remote: "file://$bare_remote"
anchor_interval_hours: 1
resume_scan_interval_seconds: 1
outbox_dispatch_interval_seconds: 1
EOF
)"
	if ! start_daemon scenario-c "$cfg"; then
		RESULT_C=FAIL; fail "C: ledger tamper detected vs remote anchor" "daemon never became healthy"
		return
	fi
	local daemon_pid=$D_PID sock=$D_SOCK home=$D_HOME db=$D_DB

	# >=2 real anchor pushes: Pusher.Run fires once at startup and once at
	# graceful shutdown (main.go) - the startup push on an EMPTY ledger is a
	# no-op, so two full "submit a task, then gracefully stop" cycles
	# reliably produce two real pushed anchor_log rows.
	local trace1; trace1="$(new_trace_id)"
	post_task "$sock" "$trace1" "accept_w4 scenario C seed 1"
	wait_for_task_id "$db" "$trace1" 10 >/dev/null
	stop_daemon "$daemon_pid"

	if ! launch_daemon scenario-c; then
		RESULT_C=FAIL; fail "C: ledger tamper detected vs remote anchor" "daemon never became healthy on restart (seed 2)"
		return
	fi
	daemon_pid=$D_PID

	local trace2; trace2="$(new_trace_id)"
	post_task "$sock" "$trace2" "accept_w4 scenario C seed 2"
	wait_for_task_id "$db" "$trace2" 10 >/dev/null
	stop_daemon "$daemon_pid"

	local pushed_count
	pushed_count="$(sqlite3 "$db" "SELECT COUNT(*) FROM anchor_log WHERE status='pushed';")"
	log "C: pushed anchor_log rows = $pushed_count (want >=2)"
	if [ "$pushed_count" -lt 2 ]; then
		RESULT_C=FAIL; fail "C: ledger tamper detected vs remote anchor" "fewer than 2 anchors pushed"
		return
	fi

	# Tamper (kahyad stopped, uncontended): DROP the append-only trigger
	# (raw-connection attacker threat model - exactly W4-05's own
	# verify_test.go tamper test), then mutate the earliest event's payload.
	sqlite3 "$db" "DROP TRIGGER events_no_update;"
	sqlite3 "$db" "UPDATE events SET payload=json_set(payload,'\$.k','tampered') WHERE id=(SELECT MIN(id) FROM events);"
	log "C: tampered with event id=$(sqlite3 "$db" 'SELECT MIN(id) FROM events;')"

	if ! launch_daemon scenario-c; then
		RESULT_C=FAIL; fail "C: ledger tamper detected vs remote anchor" "daemon never became healthy after tamper"
		return
	fi
	daemon_pid=$D_PID

	local verify_out verify_code
	verify_out="$(cli "$sock" "$home" ledger verify 2>&1)"
	verify_code=$?
	log "C: kahya ledger verify exit=$verify_code output=$verify_out"

	local mismatch_events
	mismatch_events="$(sqlite3 "$db" "SELECT COUNT(*) FROM events WHERE kind='anchor.mismatch';")"

	if [ "$verify_code" -ne 0 ] && echo "$verify_out" | grep -q "DEFTER UYARISI" && [ "$mismatch_events" -ge 1 ]; then
		RESULT_C=PASS; pass "C: ledger tamper detected vs remote anchor"
	else
		RESULT_C=FAIL; fail "C: ledger tamper detected vs remote anchor" "exit=$verify_code mismatch_events=$mismatch_events"
	fi

	stop_daemon "$daemon_pid"
}

log ""
log "--- Scenario A: kill-resume no-double-execution ---"
run_scenario_a
log ""
log "--- Scenario B: offline -> reconnect ---"
run_scenario_b
log ""
log "--- Scenario C: ledger tamper vs remote anchor ---"
run_scenario_c

# --- W4_REAL evidence event (task spec step 7) ---
if [ "$W4_REAL" = "1" ] && [ "$RESULT_A" = "PASS" ]; then
	log ""
	log "--- W4_REAL: recording accept.w4_real_run evidence ---"
	# Recorded into the SAME scenario-A dev brain.db this run just used -
	# a deliberate, narrow exception to "kahyad is brain.db's only writer"
	# (tasks/README.md): this is an ACCEPTANCE-GATE evidence marker written
	# into a throwaway DEV database after the daemon has already stopped,
	# never a production write, and there is no existing kahyad HTTP route
	# for "log an arbitrary custom event" (deliberately - that would be a
	# real security hole) to do this any other way.
	SCENARIO_A_DB="$SCRATCH_BASE/scenario-a/home/Library/Application Support/Kahya-dev/brain.db"
	PAYLOAD="$("$PYTHON3" -c 'import json,sys; print(json.dumps({"duration_s": int(sys.argv[1]), "scenario_results": {"A": sys.argv[2], "B": sys.argv[3], "C": sys.argv[4]}}))' \
		"$SCENARIO_A_DURATION_S" "$RESULT_A" "$RESULT_B" "$RESULT_C")"
	NOW="$("$PYTHON3" -c 'import datetime; print(datetime.datetime.now(datetime.timezone.utc).isoformat())')"
	sqlite3 "$SCENARIO_A_DB" "INSERT INTO events (trace_id, ts, kind, payload, created_at) VALUES ('accept-w4-real-run', '$NOW', 'accept.w4_real_run', '$(printf '%s' "$PAYLOAD" | sed "s/'/''/g")', '$NOW');"
	log "accept.w4_real_run recorded in $SCENARIO_A_DB"
fi

# --- PROD SAFETY final check ---
PROD_MAX_AFTER="$(prod_max_event_id)"
log ""
log "prod brain.db MAX(events.id) after run: $PROD_MAX_AFTER (before was $PROD_MAX_BEFORE)"
if [ "$PROD_MAX_BEFORE" != "$PROD_MAX_AFTER" ]; then
	log "PROD SAFETY VIOLATION: prod brain.db event count changed during this run ($PROD_MAX_BEFORE -> $PROD_MAX_AFTER)"
	FAILS=$((FAILS+1))
fi

log ""
log "=== SUMMARY ==="
log "  A: kill-resume no-double-execution:     ${RESULT_A:-FAIL (never ran)}"
log "  B: offline -> reconnect completes:      ${RESULT_B:-FAIL (never ran)}"
log "  C: ledger tamper detected vs anchor:    ${RESULT_C:-FAIL (never ran)}"
log "  total failures (scenarios + prod-safety): $FAILS"

if [ "$FAILS" -gt 0 ]; then
	exit 1
fi
exit 0
