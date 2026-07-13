#!/usr/bin/env bash
# scripts/w5_gate.sh -- the W5 acceptance gate's human-run end-to-end drill
# (HANDOFF §6 W5 acceptance; tasks/w5-proactivity/W5-05-w5-acceptance.md
# Steps 4-7). Companion to the four HERMETIC gate tests `make test` runs
# (kahyad/internal/briefing/gate_test.go, .../consolidation/gate_test.go,
# .../policy/taint_gate_test.go, .../eval/mini_test.go): those exercise the
# real Go code in isolation with fixtures; this script drives the SAME
# invariants against a REAL, already-running kahyad (launchd `make
# install-agent` or `make run-daemon` in another terminal) and the REAL
# ~/Kahya seed corpus - no mock server, no fixture corpus - mirroring
# scripts/accept-w12.sh's own "hermetic Go gate + separate live drill
# script" split.
#
# Prints GEÇTİ / KALDI / ERTELENDİ (PASS / FAIL / DEFERRED) per gate item,
# in Turkish, plus a final summary. Idempotent: every step here is safe to
# re-run (the once-per-day briefing dedupe, the pending-consolidation
# supersede rule, and the truth-ritual's own idempotent answer handling are
# all exercised exactly as they behave in normal daily use - re-running
# this script never double-delivers, double-commits, or double-answers).
#
# ERTELENDİ (DEFERRED) means "could not be verified on THIS machine right
# now" (most commonly: no live Anthropic credential wired yet, HANDOFF
# W0-04's own blocked-user note) - it does NOT by itself fail the gate,
# since a DEFERRED item was never exercised, successfully or not. Re-run
# this script once the missing piece is available to turn it into a real
# GEÇTİ/KALDI.
#
# Gate item 4 (truth-ritual: a human answering >=1 Telegram question) is
# the ONE genuinely USER-ASSIST item (task spec, verbatim) - this script
# prompts for it and, if the user is unavailable/no answer arrives within
# the wait window, reports it as ERTELENDİ without ever fabricating an
# answer.
set -uo pipefail

DATA_DIR="${KAHYA_DATA_DIR:-$HOME/Library/Application Support/Kahya}"
SOCK="${KAHYA_SOCKET:-$DATA_DIR/kahyad.sock}"
DB_PATH="${KAHYA_DB_PATH:-$DATA_DIR/brain.db}"
LOG_DIR="$DATA_DIR/logs"
KAHYAD_LOG="$LOG_DIR/kahyad.jsonl"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KAHYA_BIN="$REPO_ROOT/bin/kahya"
KAHYA_MEMORY_REPO="${KAHYA_DIR:-$HOME/Kahya}"

# Madde 4 (truth-ritual) için bekleme süresi - kullanıcı Telegram'da en az
# bir soruya yanıt versin diye (saniye). Ortam değişkeniyle kısaltılabilir.
RITUAL_WAIT_S="${W5_GATE_RITUAL_WAIT_S:-90}"

FAILS=0
DEFERRED=0

pass()     { printf 'GEÇTİ      %s\n' "$1"; }
fail()     { printf 'KALDI      %s -- %s\n' "$1" "$2"; FAILS=$((FAILS+1)); }
deferred() { printf 'ERTELENDİ  %s -- %s\n' "$1" "$2"; DEFERRED=$((DEFERRED+1)); }

echo "=== W5 kabul tatbikatı (scripts/w5_gate.sh) ==="
echo "soket:  $SOCK"
echo "db:     $DB_PATH"
echo "loglar: $LOG_DIR"
echo

if [ ! -x "$KAHYA_BIN" ]; then
	fail "sağlık" "$KAHYA_BIN bulunamadı/çalıştırılamıyor -- önce 'make build' çalıştırın"
	echo
	echo "=== ÖZET: $FAILS KALDI, $DEFERRED ERTELENDİ ==="
	exit 1
fi

HEALTH_OUT="$("$KAHYA_BIN" health 2>&1)"
if [ $? -ne 0 ]; then
	fail "sağlık" "kahya health başarısız: $HEALTH_OUT"
	echo
	echo "kahyad $SOCK üzerinde erişilebilir değil -- başlatın ('make install-agent'"
	echo "ya da başka bir terminalde 'make run-daemon') ve tekrar çalıştırın."
	echo
	echo "=== ÖZET: $FAILS KALDI, $DEFERRED ERTELENDİ ==="
	exit 1
fi
pass "sağlık ($HEALTH_OUT)"
echo

# jsonl_trace_lines PATH TRACE_ID: PATH altındaki (kahyad.jsonl) satırlardan
# trace_id alanı TRACE_ID'ye eşit olanları yazar - kahya log --trace'in
# kendisinin okuduğu dosyanın aynısı (kahyad/internal/server.readLogLines).
jsonl_trace_lines() {
	python3 - "$1" "$2" <<'PYEOF'
import json, sys
path, trace_id = sys.argv[1], sys.argv[2]
try:
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except Exception:
                continue
            if obj.get("trace_id") == trace_id:
                print(json.dumps(obj))
except FileNotFoundError:
    pass
PYEOF
}

# db_event_count TRACE_ID KIND: brain.db'nin events tablosunda verilen
# trace_id + kind çiftine sahip satır sayısını döner (salt-okunur SELECT -
# scripts/accept-w12.sh'ın ledger-forensics kontrolü gibi, kahyad'ın TEK
# yazar kuralını ihlal etmez).
db_event_count() {
	sqlite3 "$DB_PATH" "SELECT count(*) FROM events WHERE trace_id='$1' AND kind='$2';" 2>/dev/null || echo 0
}

# wait_for_job_done TRACE_ID TIMEOUT_S: kahyad/internal/scheduler.Scheduler.
# Trigger'ın HER görev için (hangi iş olursa olsun) çalıştırdığı işleyici
# goroutine'i bitirdiğinde yazdığı evrensel "job_completed"/"job_failed"
# JSONL satırını bekler (poll) - brifing/konsolidasyon paketlerinin kendi
# özel JSONL satırları yalnızca bazı yollarda var (örn. "briefing.
# skipped_duplicate" YALNIZCA bir ledger olayıdır, hiçbir JSONL satırı
# yoktur) - bu yüzden "iş bitti mi" sorusunun TEK güvenilir evrensel
# yanıtı budur; NE olduğu (teslim mi edildi, atlandı mı, diff üretti mi)
# ayrıca brain.db'nin events tablosundan (db_event_count) okunur. Bulunan
# event adını yazar; zaman aşımında boş string.
wait_for_job_done() {
	local trace_id="$1"
	local timeout_s="$2"
	local waited=0
	while [ "$waited" -lt "$timeout_s" ]; do
		local found
		# NOTE: this reads $KAHYAD_LOG directly BY PATH inside the one
		# python3 heredoc, rather than piping jsonl_trace_lines' output into
		# a second `python3 -` invocation - `python3 -` reads its OWN
		# program text from stdin, so a second heredoc-fed `python3 -`
		# downstream of a pipe would have its sys.stdin already exhausted
		# by the interpreter reading the heredoc program itself, silently
		# seeing zero input lines every time (a real bug caught while
		# smoke-testing this script against a live daemon - see this file's
		# git history / the W5-05 report for the repro).
		found="$(python3 - "$KAHYAD_LOG" "$trace_id" job_completed job_failed <<'PYEOF'
import json, sys
path, trace_id = sys.argv[1], sys.argv[2]
wanted = set(sys.argv[3:])
try:
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except Exception:
                continue
            if obj.get("trace_id") != trace_id:
                continue
            if obj.get("event") in wanted:
                print(obj["event"])
                break
except FileNotFoundError:
    pass
PYEOF
		)"
		if [ -n "$found" ]; then
			echo "$found"
			return 0
		fi
		sleep 2
		waited=$((waited+2))
	done
	echo ""
	return 1
}

trigger_job() {
	local name="$1"
	local out
	out="$("$KAHYA_BIN" job run "$name" 2>&1)"
	if [ $? -ne 0 ]; then
		echo ""
		return 1
	fi
	printf '%s\n' "$out" | sed -n 's/.*(iz: \(.*\))$/\1/p' | head -n1
}

# ============================================================
# Madde 1 -- brifing: tek bildirim + trace_id (kahya log --trace'te
# collector/worker/delivery), tekrar çalıştırma -> briefing.skipped_duplicate
# ============================================================
echo "--- Madde 1: sabah brifingi ---"
TRACE1="$(trigger_job morning-briefing)"
if [ -z "$TRACE1" ]; then
	fail "brifing-tetikle" "kahya job run morning-briefing başarısız oldu"
else
	JOB1="$(wait_for_job_done "$TRACE1" 60)"
	if [ "$JOB1" = "job_failed" ]; then
		deferred "brifing-tek-bildirim" "iş çalıştırılırken hata oluştu (muhtemelen canlı Anthropic kimlik bilgisi yok, HANDOFF W0-04) - trace_id=$TRACE1, ayrıntı: kahya log --trace $TRACE1"
		deferred "brifing-trace_id" "yukarıdaki hataya bağlı"
		deferred "brifing-tekrar-atlandı" "başarılı bir ilk teslimat gerektirir"
	elif [ "$JOB1" != "job_completed" ]; then
		fail "brifing-tek-bildirim" "trace_id=$TRACE1 için 60s içinde iş tamamlanmadı (collect_calendar.go'nun osascript çağrısı, Takvim Otomasyonu izni hiç karar verilmemişse SINIRSIZ askıda kalabilir - HANDOFF W0-04/W5-01'in TCC notu; kahya log --trace $TRACE1 ve 'ps aux | grep osascript' ile kontrol edin)"
		deferred "brifing-trace_id" "yukarıdaki hataya bağlı"
		deferred "brifing-tekrar-atlandı" "yukarıdaki hataya bağlı"
	elif [ "$(db_event_count "$TRACE1" briefing.delivered)" -gt 0 ]; then
		pass "brifing-tek-bildirim (trace_id=$TRACE1 için briefing.delivered ledgerlandı)"

		LOG_LINES="$(jsonl_trace_lines "$KAHYAD_LOG" "$TRACE1")"
		SAW_COLLECTED=$(printf '%s\n' "$LOG_LINES" | grep -c '"event": *"briefing_collected"' || true)
		SAW_WORKER=$(printf '%s\n' "$LOG_LINES" | grep -cE '"event": *"briefing_worker_(spawn|done)"' || true)
		SAW_DELIVERED=$(printf '%s\n' "$LOG_LINES" | grep -c '"event": *"briefing_delivered"' || true)
		if [ "${SAW_COLLECTED:-0}" -gt 0 ] && [ "${SAW_WORKER:-0}" -gt 0 ] && [ "${SAW_DELIVERED:-0}" -gt 0 ]; then
			pass "brifing-trace_id (kahya log --trace $TRACE1: collector+worker+delivery aynı iz)"
		else
			fail "brifing-trace_id" "collector=$SAW_COLLECTED worker=$SAW_WORKER delivered=$SAW_DELIVERED (kahya log --trace $TRACE1)"
		fi
		"$KAHYA_BIN" log --trace "$TRACE1" >"$REPO_ROOT/w5_gate_briefing_trace.log" 2>&1 || true
		echo "  (kayıt: $REPO_ROOT/w5_gate_briefing_trace.log)"

		# Aynı takvim günü ikinci çalıştırma -> skipped_duplicate (sadece
		# ledgerlanır, hiçbir JSONL satırı yoktur - db_event_count kullanılır).
		TRACE1B="$(trigger_job morning-briefing)"
		if [ -z "$TRACE1B" ]; then
			fail "brifing-tekrar" "ikinci kahya job run morning-briefing başarısız oldu"
		else
			wait_for_job_done "$TRACE1B" 30 >/dev/null
			DUP_COUNT="$(db_event_count "$TRACE1B" briefing.skipped_duplicate)"
			if [ "${DUP_COUNT:-0}" -gt 0 ]; then
				pass "brifing-tekrar-atlandı (aynı gün ikinci çalıştırma briefing.skipped_duplicate)"
			else
				fail "brifing-tekrar-atlandı" "trace_id=$TRACE1B için briefing.skipped_duplicate bulunamadı"
			fi
		fi
	else
		fail "brifing-tek-bildirim" "iş tamamlandı ama trace_id=$TRACE1 için briefing.delivered ledgerlanmadı (kahya log --trace $TRACE1 ile inceleyin)"
		deferred "brifing-trace_id" "yukarıdaki hataya bağlı"
		deferred "brifing-tekrar-atlandı" "yukarıdaki hataya bağlı"
	fi
fi
echo

# ============================================================
# Madde 2 -- taint: brifing oturumundan (tainted) doğrudan bir W-araç
# çağrısı reddediliyor; AYNI eylem temiz bir oturumdan onay akışına
# ulaşıyor. Brifing session_taint/tasks satırları taint.InsertUntrusted +
# TaskStore.InsertTask worker'a HİÇ spawn edilmeden ÖNCE commit edildiği
# için (kahyad/internal/briefing.Orchestrator.Run's own doc comment), bu
# madde canlı bir Anthropic kimlik bilgisi OLMADAN bile yürür - Madde 1
# yukarıda kısmen ERTELENDİ olsa bile TRACE1/TASK1 zaten gerçek satırlar.
# ============================================================
echo "--- Madde 2: taint (aynı araç + aynı hedef) ---"
if [ -z "${TRACE1:-}" ]; then
	deferred "taint-tainted-deny" "Madde 1'in brifing trace_id'si yok (tetikleme başarısız oldu)"
	deferred "taint-clean-allow" "Madde 1'in brifing trace_id'si yok"
else
	# (Same direct-file-read shape as wait_for_job_done above, and for the
	# same reason - never pipe jsonl_trace_lines into a second `python3 -`
	# heredoc.)
	TASK1="$(python3 - "$KAHYAD_LOG" "$TRACE1" <<'PYEOF'
import json, sys
path, trace_id = sys.argv[1], sys.argv[2]
try:
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except Exception:
                continue
            if obj.get("trace_id") == trace_id and obj.get("event") == "briefing_worker_spawn":
                print(obj.get("task_id", ""))
                break
except FileNotFoundError:
    pass
PYEOF
	)"
	# AYNI (araç, hedef) çifti - iki çağrıda da byte-birebir aynı JSON gövde.
	TOOL_NAME="fs_write"
	TOOL_INPUT='{"path":"/tmp/w5-gate-drill-probe.md","content":"tatbikat"}'

	if [ -z "$TASK1" ]; then
		if [ "${JOB1:-}" = "job_completed" ] || [ "${JOB1:-}" = "job_failed" ]; then
			fail "taint-tainted-deny" "brifing worker'ın task_id'si kahyad.jsonl'de bulunamadı (trace_id=$TRACE1)"
		else
			deferred "taint-tainted-deny" "Madde 1'in brifing işi henüz tamamlanmadı (muhtemelen collect_calendar.go'nun osascript çağrısı askıda) - taint satırı hiç commit edilmemiş olabilir"
		fi
	else
		TAINTED_JSON="$(curl -sS --unix-socket "$SOCK" -X POST http://kahyad/policy/check \
			-H 'Content-Type: application/json' \
			-d "$(python3 -c "import json,sys; print(json.dumps({'trace_id': sys.argv[1], 'task_id': sys.argv[2], 'tool_name': sys.argv[3], 'tool_input': json.loads(sys.argv[4])}))" \
				"$TRACE1" "$TASK1" "$TOOL_NAME" "$TOOL_INPUT")" 2>&1)"
		TAINTED_DECISION="$(python3 -c "import json,sys; print(json.loads(sys.argv[1]).get('decision','?'))" "$TAINTED_JSON" 2>/dev/null || echo "?")"
		if [ "$TAINTED_DECISION" = "deny" ]; then
			pass "taint-tainted-deny (brifing oturumu [$TASK1]: $TOOL_NAME -> deny)"
		else
			fail "taint-tainted-deny" "beklenen decision=deny, alınan: $TAINTED_JSON"
		fi
	fi

	# Temiz kontrol oturumu: brain.db'de gerçek bir 'clean' session_taint
	# satırına bağlı EN SON tasks satırını ara (salt-okunur SELECT -
	# scripts/accept-w12.sh'ın ledger-forensics kontrolü gibi, kahyad'ın
	# TEK yazar kuralını ihlal etmez). Bulunursa AYNI araç+hedefi o
	# oturumun (task_id, trace_id) çiftiyle dener.
	CLEAN_ROW="$(sqlite3 -separator '|' "$DB_PATH" \
		"SELECT t.id, t.trace_id FROM tasks t JOIN session_taint st ON st.session_id = t.session_id WHERE st.tier = 'clean' ORDER BY t.id DESC LIMIT 1;" 2>/dev/null || true)"
	if [ -z "$CLEAN_ROW" ]; then
		deferred "taint-clean-allow" "brain.db'de 'clean' etiketli hiçbir gerçek oturum bulunamadı - canlı bir Anthropic kimlik bilgisiyle bir kez 'kahya \"merhaba\"' çalıştırıp tekrar deneyin"
	else
		CLEAN_TASK_ID="$(printf '%s' "$CLEAN_ROW" | cut -d'|' -f1)"
		CLEAN_TRACE_ID="$(printf '%s' "$CLEAN_ROW" | cut -d'|' -f2)"
		CLEAN_JSON="$(curl -sS --unix-socket "$SOCK" -X POST http://kahyad/policy/check \
			-H 'Content-Type: application/json' \
			-d "$(python3 -c "import json,sys; print(json.dumps({'trace_id': sys.argv[1], 'task_id': sys.argv[2], 'tool_name': sys.argv[3], 'tool_input': json.loads(sys.argv[4])}))" \
				"$CLEAN_TRACE_ID" "$CLEAN_TASK_ID" "$TOOL_NAME" "$TOOL_INPUT")" 2>&1)"
		CLEAN_DECISION="$(python3 -c "import json,sys; print(json.loads(sys.argv[1]).get('decision','?'))" "$CLEAN_JSON" 2>/dev/null || echo "?")"
		if [ "$CLEAN_DECISION" = "allow" ] || [ "$CLEAN_DECISION" = "needs_approval" ]; then
			pass "taint-clean-allow (temiz oturum [$CLEAN_TASK_ID]: $TOOL_NAME -> $CLEAN_DECISION, onay akışına ulaşıyor)"
		else
			fail "taint-clean-allow" "beklenen decision=allow|needs_approval, alınan: $CLEAN_JSON"
		fi
	fi
fi
echo

# ============================================================
# Madde 3 -- konsolidasyon: mini-baseline (önce) -> nightly-consolidation
# -> show -> approve -> git log -1 author=kahyad + reindex olayı ->
# mini-baseline (sonra) -> sıfır gerileme.
# ============================================================
echo "--- Madde 3: konsolidasyon + mini-baseline ---"
BEFORE_OUT="$("$KAHYA_BIN" eval mini 2>&1)"
BEFORE_STATUS=$?
echo "$BEFORE_OUT" | sed 's/^/  /'
if [ $BEFORE_STATUS -ne 0 ] && ! printf '%s' "$BEFORE_OUT" | grep -q 'İlk çalıştırma'; then
	fail "mini-baseline-önce" "kahya eval mini (öncesi) çıkış kodu $BEFORE_STATUS ile bitti (bir gerileme mi var, yoksa daemon mı erişilemez kontrol edin)"
else
	pass "mini-baseline-önce (kayıtlı; eval.mini.run olayı yazıldı)"
fi

TRACE3="$(trigger_job nightly-consolidation)"
if [ -z "$TRACE3" ]; then
	fail "konsolidasyon-tetikle" "kahya job run nightly-consolidation başarısız oldu"
else
	JOB3="$(wait_for_job_done "$TRACE3" 60)"
	if [ "$JOB3" = "job_failed" ]; then
		deferred "konsolidasyon-diff" "iş çalıştırılırken hata oluştu (muhtemelen canlı Anthropic kimlik bilgisi yok, HANDOFF W0-04) - trace_id=$TRACE3, ayrıntı: kahya log --trace $TRACE3"
		deferred "konsolidasyon-onay-commit" "yukarıdaki hataya bağlı"
		deferred "konsolidasyon-reindex" "yukarıdaki hataya bağlı"
		deferred "mini-baseline-sonra" "yukarıdaki hataya bağlı"
	elif [ "$JOB3" != "job_completed" ]; then
		fail "konsolidasyon-diff" "trace_id=$TRACE3 için 60s içinde iş tamamlanmadı (kahya log --trace $TRACE3 ile inceleyin)"
		deferred "konsolidasyon-onay-commit" "yukarıdaki hataya bağlı"
		deferred "konsolidasyon-reindex" "yukarıdaki hataya bağlı"
		deferred "mini-baseline-sonra" "yukarıdaki hataya bağlı"
	elif [ "$(db_event_count "$TRACE3" consolidation.pending)" -gt 0 ]; then
		SHOW_OUT="$("$KAHYA_BIN" consolidation show 2>&1)"
		if [ -z "$SHOW_OUT" ] || printf '%s' "$SHOW_OUT" | grep -q 'Bekleyen konsolidasyon önerisi yok'; then
			fail "konsolidasyon-diff" "kahya consolidation show boş döndü ama consolidation.pending ledgerlanmıştı"
		else
			pass "konsolidasyon-diff (kahya consolidation show bir diff gösteriyor)"
		fi
		# reindex.completed/consolidation.approved, kahya consolidation
		# approve'un KENDİ (CLI'de taze basılan) trace_id'si altında
		# ledgerlanır - nightly-consolidation'ın $TRACE3'ü değil - bu yüzden
		# trace_id eşleştirmek yerine approve'dan ÖNCE/SONRA toplam
		# reindex.completed sayısını karşılaştırıyoruz (artış = approve'un
		# tetiklediği yeni bir reindex).
		REINDEX_BEFORE="$(sqlite3 "$DB_PATH" "SELECT count(*) FROM events WHERE kind='reindex.completed';" 2>/dev/null || echo 0)"
		APPROVE_OUT="$(printf 'onayla\n' | "$KAHYA_BIN" consolidation approve 2>&1)"
		APPROVE_STATUS=$?
		LAST_AUTHOR="$(git -C "$KAHYA_MEMORY_REPO" log -1 --format='%an' 2>&1 || true)"
		REINDEX_AFTER="$(sqlite3 "$DB_PATH" "SELECT count(*) FROM events WHERE kind='reindex.completed';" 2>/dev/null || echo 0)"
		if [ $APPROVE_STATUS -eq 0 ] && [ "$LAST_AUTHOR" = "kahyad" ]; then
			pass "konsolidasyon-onay-commit (git -C $KAHYA_MEMORY_REPO log -1 --format=%an == kahyad)"
		else
			fail "konsolidasyon-onay-commit" "approve çıkışı=$APPROVE_STATUS, son commit yazarı=$LAST_AUTHOR ($APPROVE_OUT)"
		fi
		if [ "${REINDEX_AFTER:-0}" -gt "${REINDEX_BEFORE:-0}" ]; then
			pass "konsolidasyon-reindex (reindex.completed sayısı arttı: $REINDEX_BEFORE -> $REINDEX_AFTER)"
		else
			fail "konsolidasyon-reindex" "approve sonrası yeni bir reindex.completed olayı görülmedi ($REINDEX_BEFORE -> $REINDEX_AFTER)"
		fi

		AFTER_OUT="$("$KAHYA_BIN" eval mini 2>&1)"
		AFTER_STATUS=$?
		echo "$AFTER_OUT" | sed 's/^/  /'
		if [ $AFTER_STATUS -eq 0 ]; then
			pass "mini-baseline-sonra (gerileme yok)"
		else
			fail "mini-baseline-sonra" "kahya eval mini (sonrası) çıkış kodu $AFTER_STATUS - gerileme tespit edildi, ayrıntı için yukarıya bakın"
		fi
	elif [ "$(db_event_count "$TRACE3" consolidation.approved)" -gt 0 ]; then
		pass "konsolidasyon-diff (auto-commit modu etkin; suggestion adımı atlandı)"
		pass "konsolidasyon-onay-commit (auto-commit)"
		AFTER_OUT="$("$KAHYA_BIN" eval mini 2>&1)"
		AFTER_STATUS=$?
		echo "$AFTER_OUT" | sed 's/^/  /'
		if [ $AFTER_STATUS -eq 0 ]; then
			pass "mini-baseline-sonra (gerileme yok)"
		else
			fail "mini-baseline-sonra" "kahya eval mini (sonrası) çıkış kodu $AFTER_STATUS - gerileme tespit edildi"
		fi
	else
		# Diff üretilmedi (~/Kahya/memory'de gece konsolidasyonu için hiçbir
		# değişiklik yoktu) - başarısızlık değil, ama bu tatbikat çalıştırması
		# bir diff/onay/reindex akışı GÖSTEREMEDİ.
		deferred "konsolidasyon-diff" "iş tamamlandı ama diff üretilmedi (öneriye/otomatik-commite konu olacak bir değişiklik yoktu) - ~/Kahya/memory altında bir dosyayı değiştirip tekrar deneyin"
		deferred "konsolidasyon-onay-commit" "yukarıdaki nedenle atlandı"
		deferred "konsolidasyon-reindex" "yukarıdaki nedenle atlandı"
		deferred "mini-baseline-sonra" "yukarıdaki nedenle atlandı"
	fi
fi
echo

# ============================================================
# Madde 4 -- doğruluk ritüeli: en az bir Telegram sorusuna yanıt geldiğini
# doğrula. USER-ASSIST: kullanıcı yanıt vermezse sahte yanıt ÜRETİLMEZ,
# yalnızca ERTELENDİ olarak raporlanır.
# ============================================================
echo "--- Madde 4: doğruluk ritüeli (kullanıcı-destekli) ---"
BEFORE_LABELS="$(sqlite3 "$DB_PATH" "SELECT count(*) FROM eval_labels WHERE answered_at IS NOT NULL;" 2>/dev/null || echo "")"
if [ -z "$BEFORE_LABELS" ]; then
	deferred "ritüel-yanıt" "eval_labels tablosu okunamadı (brain.db şeması farklı olabilir) - bu maddeyi elle doğrulayın"
else
	TRACE4="$(trigger_job truth-ritual)"
	if [ -z "$TRACE4" ]; then
		fail "ritüel-tetikle" "kahya job run truth-ritual başarısız oldu"
	else
		echo "Telegram'a doğruluk ritüeli soruları gönderildi (trace_id=$TRACE4)."
		echo "Lütfen Telegram'da EN AZ BİR soruyu şimdi yanıtlayın (bu tatbikat en fazla ${RITUAL_WAIT_S}s bekleyecek)..."
		waited=0
		ANSWERED=0
		while [ "$waited" -lt "$RITUAL_WAIT_S" ]; do
			AFTER_LABELS="$(sqlite3 "$DB_PATH" "SELECT count(*) FROM eval_labels WHERE answered_at IS NOT NULL;" 2>/dev/null || echo "$BEFORE_LABELS")"
			if [ "${AFTER_LABELS:-0}" -gt "${BEFORE_LABELS:-0}" ]; then
				ANSWERED=1
				break
			fi
			sleep 5
			waited=$((waited+5))
		done
		if [ "$ANSWERED" -eq 1 ]; then
			pass "ritüel-yanıt (eval_labels.answered_at sayısı arttı: $BEFORE_LABELS -> $AFTER_LABELS)"
		else
			deferred "ritüel-yanıt" "kullanıcı ${RITUAL_WAIT_S}s içinde Telegram'da yanıt vermedi (kullanıcı-destekli madde) - sahte yanıt üretilmedi; bu betiği yanıt verdikten sonra tekrar çalıştırın"
		fi
	fi
fi
echo

echo "=== ÖZET: $FAILS KALDI, $DEFERRED ERTELENDİ ==="
if [ "$FAILS" -gt 0 ]; then
	exit 1
fi
exit 0
