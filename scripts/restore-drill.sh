#!/usr/bin/env bash
# scripts/restore-drill.sh -- the W78-05 LIVE backup restore drill
# (`make restore-drill`, HANDOFF §6 W7-8 acceptance ⚑). It rehearses a full
# restore on an ISOLATED scratch profile and proves data integrity: a fresh
# backup + a same-point clone of ~/Kahya, brought up under KAHYA_ENV=restore,
# yields the SAME <hafiza> injection for the same query as production, and the
# ledger (events) / episodes -- which markdown cannot regenerate -- survive the
# VACUUM copy.
#
# The hermetic Go drill (kahyad/internal/restore.drill_test.go) proves the same
# backup -> restore -> reindex -> inject equivalence + ledger survival with
# fixtures under `make test`. THIS script is the real-daemon rehearsal against
# the actual ~/Kahya + a running production kahyad; it needs the private remote
# and a live prod daemon, so it is user-assist runtime, not part of `make test`.
#
# HARD CONSTRAINTS (fail-closed):
#   - It NEVER opens/modifies the production brain.db for writing. The restore
#     runs entirely on the KAHYA_ENV=restore scratch profile
#     (~/Library/Application Support/Kahya-restore + ~/Kahya-restore, own
#     socket). The only production touchpoints are READ-ONLY: triggering a
#     fresh W4-06 backup + the reference equivalence query over the prod UDS,
#     a `sqlite3 -readonly` read of the prod brain.db for the reference
#     ledger/episodes counts + user_version (WAL: a reader never blocks the
#     live writer), and -- on success -- POSTing the counts/hashes-only result
#     back over the prod UDS so PRODUCTION kahyad (the sole brain.db writer)
#     records the restore.drill.result row.
#   - A guard refuses to run if the scratch target resolves to the production
#     App Support dir or ~/Kahya (mirrors kahyad/internal/restore.GuardNotProd
#     and config.refuseNonProdProfileOpeningProdDB).
#   - Drill artifacts that contain memory content (the reference <hafiza> block
#     in reference.json) are written under ~/Kahya/backups/drill/ ONLY -- never
#     committed to this code repo.
#
# Exit: nonzero on any failed check.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KAHYAD_BIN="$REPO_ROOT/bin/kahyad"

PYTHON3="$(command -v python3 || true)"
if [ -z "$PYTHON3" ]; then
	echo "python3 not found on PATH -- required for JSON parsing/normalization" >&2
	exit 2
fi
if [ ! -x "$KAHYAD_BIN" ]; then
	echo "$KAHYAD_BIN not found/executable -- run 'make build' first" >&2
	exit 2
fi

EQUIV_QUERY="${EQUIV_QUERY:-evlerimizden}"
EQUIV_K="${EQUIV_K:-8}"

# --- path resolution (defaults; a config.yaml override would move them, but
# this drill hardcodes the standard layout and then GUARDS it) ----------------
PROD_APP_SUPPORT="$HOME/Library/Application Support/Kahya"
PROD_SOCK="$PROD_APP_SUPPORT/kahyad.sock"
PROD_DB="$PROD_APP_SUPPORT/brain.db"
PROD_KAHYA="$HOME/Kahya"
BACKUP_DIR="$PROD_KAHYA/backups"
DRILL_ARTIFACT_DIR="$BACKUP_DIR/drill"
REFERENCE_JSON="$DRILL_ARTIFACT_DIR/reference.json"

RESTORE_APP_SUPPORT="$HOME/Library/Application Support/Kahya-restore"
RESTORE_SOCK="$RESTORE_APP_SUPPORT/kahyad-restore.sock"
RESTORE_DB="$RESTORE_APP_SUPPORT/brain.db"
RESTORE_KAHYA="$HOME/Kahya-restore"
RESTORE_MEM="$RESTORE_KAHYA/memory"

# --- FAIL-CLOSED guard: the scratch target must never BE production ----------
if [ "$RESTORE_APP_SUPPORT" = "$PROD_APP_SUPPORT" ] || \
   [ "$RESTORE_DB" = "$PROD_APP_SUPPORT/brain.db" ] || \
   [ "$RESTORE_KAHYA" = "$PROD_KAHYA" ] || \
   [ "$RESTORE_MEM" = "$PROD_KAHYA/memory" ]; then
	echo "REFUSING: the restore scratch target resolves to a PRODUCTION path." >&2
	echo "  scratch app-support: $RESTORE_APP_SUPPORT" >&2
	echo "  scratch brain.db:    $RESTORE_DB" >&2
	echo "  scratch memory:      $RESTORE_MEM" >&2
	echo "The drill must never clobber production data (HANDOFF §6 backup ⚑)." >&2
	exit 3
fi

fail() { echo "DRILL FAIL: $*" >&2; exit 1; }

echo "=== W78-05 restore drill (make restore-drill) -- $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="

# --- 0. production kahyad must be reachable ----------------------------------
if [ ! -S "$PROD_SOCK" ] || ! curl -s --unix-socket "$PROD_SOCK" -o /dev/null -w '%{http_code}' http://x/health 2>/dev/null | grep -q '^200$'; then
	fail "production kahyad not reachable at $PROD_SOCK -- start it ('make install-agent' or 'make run-daemon') and re-run."
fi

# --- 1a. capture the PRODUCTION-LIVE ledger counts + schema version BEFORE the
# backup (read-only). These are the reference LOWER BOUNDS: the VACUUM snapshot
# taken next reflects prod at-or-after this instant, so a faithful backup can
# only have >= these rows. Reading them from the BACKUP instead (as a naive
# drill does) would be circular -- the restored db is a byte copy of the backup,
# so restored-vs-backup counts are equal by construction and could never catch a
# VACUUM that dropped the ledger. Reading prod-live-BEFORE-backup makes the
# `restored >= reference` check able to fail. `-readonly` guarantees the drill
# never writes prod brain.db (WAL: a reader never blocks the live writer).
echo "==> reading production reference counts + schema version (read-only)"
PROD_COUNTS="$(sqlite3 -readonly "$PROD_DB" 'SELECT (SELECT count(*) FROM events) || "," || (SELECT count(*) FROM episodes);')" \
	|| fail "reading production ledger/episodes counts from $PROD_DB failed"
REF_EVENTS="${PROD_COUNTS%%,*}"
REF_EPISODES="${PROD_COUNTS##*,}"
PROD_UV="$(sqlite3 -readonly "$PROD_DB" 'PRAGMA user_version;')" || fail "reading production user_version failed"

# --- 1b. trigger a FRESH W4-06 backup on production (VACUUM INTO + verify) ----
echo "==> triggering a fresh backup-nightly on production"
# The newest pre-existing backup's mtime (epoch), so we can wait for a STRICTLY
# newer file rather than racing a fixed sleep (backups are named by date only,
# so a same-day re-trigger overwrites the file in place -> detect by mtime).
prior_backup="$(ls -1 "$BACKUP_DIR"/brain-*.db 2>/dev/null | sort | tail -1)"
PRIOR_MTIME=0
[ -n "$prior_backup" ] && PRIOR_MTIME="$(stat -f %m "$prior_backup" 2>/dev/null || echo 0)"
TRIG_CODE="$(curl -s --unix-socket "$PROD_SOCK" -o /dev/null -w '%{http_code}' -X POST http://x/jobs/trigger/backup-nightly)"
if [ "$TRIG_CODE" != "202" ]; then
	fail "backup-nightly trigger returned HTTP $TRIG_CODE, want 202"
fi
# Poll (bounded) for a brain-*.db strictly newer than the pre-trigger one, so a
# slow VACUUM can never make us rehearse a stale backup.
BACKUP_FILE=""
for _ in $(seq 1 60); do
	cand="$(ls -1 "$BACKUP_DIR"/brain-*.db 2>/dev/null | sort | tail -1)"
	if [ -n "$cand" ]; then
		cand_mtime="$(stat -f %m "$cand" 2>/dev/null || echo 0)"
		if [ "$cand_mtime" -gt "$PRIOR_MTIME" ]; then
			BACKUP_FILE="$cand"
			break
		fi
	fi
	sleep 1
done
[ -n "$BACKUP_FILE" ] || fail "no FRESH brain-*.db backup appeared in $BACKUP_DIR within 60s of the trigger"
echo "    fresh backup: $BACKUP_FILE"

# --- 1c. record the PRODUCTION reference <hafiza> block at the same point -----
echo "==> recording production reference (injected block)"
mkdir -p "$DRILL_ARTIFACT_DIR"
REF_RESP="$(curl -sS --unix-socket "$PROD_SOCK" -X POST http://x/v1/memory/search \
	-H 'Content-Type: application/json' \
	-d "{\"query\":\"$EQUIV_QUERY\",\"k\":$EQUIV_K,\"for_injection\":true,\"task_id\":\"restore-drill\"}")" \
	|| fail "prod equivalence query failed"
REF_BLOCK="$(printf '%s' "$REF_RESP" | "$PYTHON3" -c 'import json,sys; print(json.load(sys.stdin).get("hafiza_block",""))')"
[ -n "$REF_BLOCK" ] || fail "prod reference <hafiza> block is empty -- the equivalence query retrieved nothing"
# reference.json holds memory content (the block) -> artifact dir ONLY, never
# git. The block is passed via a base64 env var to sidestep shell-quoting
# pitfalls with multi-line / Turkish content.
REF_BLOCK_B64="$(printf '%s' "$REF_BLOCK" | base64)"
REF_BLOCK_B64="$REF_BLOCK_B64" BF="$BACKUP_FILE" EV="$REF_EVENTS" EP="$REF_EPISODES" "$PYTHON3" - "$REFERENCE_JSON" <<'PYEOF'
import base64, json, os, sys
block = base64.b64decode(os.environ["REF_BLOCK_B64"]).decode("utf-8")
with open(sys.argv[1], "w") as f:
    json.dump({"backup_file": os.environ["BF"], "events": int(os.environ["EV"]),
               "episodes": int(os.environ["EP"]), "block": block}, f)
PYEOF
echo "    reference: events=$REF_EVENTS episodes=$REF_EPISODES -> $REFERENCE_JSON"

# --- 2. fresh clone of ~/Kahya into the scratch memory repo ------------------
echo "==> cloning $PROD_KAHYA -> $RESTORE_KAHYA"
rm -rf "$RESTORE_KAHYA"
git clone --quiet "$PROD_KAHYA" "$RESTORE_KAHYA" || fail "git clone $PROD_KAHYA failed"

# --- 3. place the backup at the scratch brain.db path ------------------------
echo "==> restoring backup -> $RESTORE_DB"
rm -rf "$RESTORE_APP_SUPPORT"
mkdir -p "$RESTORE_APP_SUPPORT"
cp "$BACKUP_FILE" "$RESTORE_DB"

# --- 4. integrity_check ------------------------------------------------------
INTEG="$(sqlite3 "$RESTORE_DB" 'PRAGMA integrity_check;')"
[ "$INTEG" = "ok" ] || fail "restored db integrity_check = '$INTEG', want 'ok'"
echo "    integrity_check: ok"

# --- 5. boot the scratch kahyad (migrations run; verify user_version) --------
echo "==> booting scratch kahyad (KAHYA_ENV=restore)"
KAHYA_ENV=restore "$KAHYAD_BIN" >/tmp/kahyad-restore-drill.log 2>&1 &
RESTORE_PID=$!
cleanup() {
	kill "$RESTORE_PID" 2>/dev/null
	wait "$RESTORE_PID" 2>/dev/null
}
trap cleanup EXIT

for _ in $(seq 1 50); do
	if [ -S "$RESTORE_SOCK" ] && curl -s --unix-socket "$RESTORE_SOCK" -o /dev/null -w '%{http_code}' http://x/health 2>/dev/null | grep -q '^200$'; then
		break
	fi
	sleep 0.2
done
[ -S "$RESTORE_SOCK" ] || fail "scratch kahyad did not come up on $RESTORE_SOCK (see /tmp/kahyad-restore-drill.log)"
USER_VERSION="$(sqlite3 -readonly "$RESTORE_DB" 'PRAGMA user_version;')"
# Must EQUAL production's schema version (both ran the same binary's goose
# migrations): a mere `>0` is a tautology since the scratch kahyad just migrated
# at boot, and would not catch a backup restored under a divergent/rolled-back
# migration set.
[ "$USER_VERSION" = "$PROD_UV" ] || fail "restored user_version = $USER_VERSION, want production's $PROD_UV (schema mismatch)"
echo "    user_version: $USER_VERSION (matches production)"

# --- 6. reindex from the cloned markdown -- must be an incremental no-op ------
echo "==> reindexing scratch from $RESTORE_MEM (expect no-op)"
REIDX="$(curl -sS --unix-socket "$RESTORE_SOCK" -X POST http://x/v1/reindex \
	-H 'Content-Type: application/json' -d '{"full":false}')" || fail "scratch reindex failed"
# A clean no-op = nothing added/removed/rechunked AND nothing errored AND at
# least one file was seen-and-unchanged. Omitting files_errored would let a
# clone with a skipped symlink / unreadable md file masquerade as a no-op;
# requiring files_unchanged>0 rejects an empty/zero-file reindex.
NOOP="$(printf '%s' "$REIDX" | "$PYTHON3" -c 'import json,sys; d=json.load(sys.stdin); print("OK" if (d.get("files_indexed",0)==0 and d.get("files_removed",0)==0 and d.get("chunks",0)==0 and d.get("files_errored",0)==0 and d.get("files_unchanged",0)>0) else "DRIFT")')"
[ "$NOOP" = "OK" ] || fail "reindex was NOT a clean no-op (markdown<->index drift, an errored file, or an empty corpus): $REIDX"
echo "    reindex: clean no-op"

# --- 7. equivalence query through the scratch daemon -------------------------
echo "==> running equivalence query through scratch kahyad"
RESTORE_RESP="$(curl -sS --unix-socket "$RESTORE_SOCK" -X POST http://x/v1/memory/search \
	-H 'Content-Type: application/json' \
	-d "{\"query\":\"$EQUIV_QUERY\",\"k\":$EQUIV_K,\"for_injection\":true,\"task_id\":\"restore-drill\"}")" \
	|| fail "scratch equivalence query failed"
RESTORE_BLOCK="$(printf '%s' "$RESTORE_RESP" | "$PYTHON3" -c 'import json,sys; print(json.load(sys.stdin).get("hafiza_block",""))')"

# --- 8. normalize ONLY trace_id + timestamps, then byte-compare --------------
# Mirrors kahyad/internal/restore.Normalize EXACTLY: 32-hex trace_id -> <TRACE_ID>,
# RFC3339/Nano timestamp -> <TS>; nothing else is masked.
normalize() {
	"$PYTHON3" - <<'PYEOF'
import re, sys
b = sys.stdin.read()
b = re.sub(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})', '<TS>', b)
b = re.sub(r'\b[0-9a-f]{32}\b', '<TRACE_ID>', b)
sys.stdout.write(b)
PYEOF
}
REF_NORM="$(printf '%s' "$REF_BLOCK" | normalize)"
RESTORE_NORM="$(printf '%s' "$RESTORE_BLOCK" | normalize)"
if [ "$REF_NORM" != "$RESTORE_NORM" ]; then
	echo "--- reference (normalized) ---" >&2
	printf '%s\n' "$REF_NORM" >&2
	echo "--- restored (normalized) ---" >&2
	printf '%s\n' "$RESTORE_NORM" >&2
	fail "restored <hafiza> block differs from the production reference after normalization"
fi
echo "    equivalence: <hafiza> byte-identical after normalization"
REF_QUERY_SHA="$(printf '%s' "$REF_NORM" | shasum -a 256 | awk '{print $1}')"

# --- 9. ledger/episodes survived (restored counts >= reference) --------------
R_COUNTS="$(sqlite3 -readonly "$RESTORE_DB" 'SELECT (SELECT count(*) FROM events) || "," || (SELECT count(*) FROM episodes);')"
R_EVENTS="${R_COUNTS%%,*}"
R_EPISODES="${R_COUNTS##*,}"
[ "$R_EVENTS" -ge "$REF_EVENTS" ] || fail "restored events $R_EVENTS < reference $REF_EVENTS (ledger did not survive)"
[ "$R_EPISODES" -ge "$REF_EPISODES" ] || fail "restored episodes $R_EPISODES < reference $REF_EPISODES (episodes did not survive)"
echo "    ledger survives: events $R_EVENTS>=$REF_EVENTS, episodes $R_EPISODES>=$REF_EPISODES"

# --- 10. report the result to PRODUCTION kahyad over its UDS ------------------
# kahyad (the sole brain.db writer) records restore.drill.result; this script
# never writes SQL to production brain.db itself.
BACKUP_BASENAME="$(basename "$BACKUP_FILE")"
echo "==> recording restore.drill.result on production kahyad"
REC_CODE="$(curl -s --unix-socket "$PROD_SOCK" -o /dev/null -w '%{http_code}' -X POST http://x/v1/restore/drill-result \
	-H 'Content-Type: application/json' \
	-d "{\"ok\":true,\"ref_query_sha\":\"$REF_QUERY_SHA\",\"backup_file\":\"$BACKUP_BASENAME\"}")"
[ "$REC_CODE" = "200" ] || fail "recording restore.drill.result returned HTTP $REC_CODE, want 200"

echo "=== DRILL PASS: restore of $BACKUP_BASENAME yields identical injection + intact ledger ==="
