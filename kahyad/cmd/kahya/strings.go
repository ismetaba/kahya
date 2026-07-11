// strings.go collects every Turkish user-facing string the kahya CLI can
// print, as named constants, in one reviewable file (W12-06 deliverable +
// HANDOFF §3 language policy: chat/UI is Turkish-first, technical output —
// code, logs, model IDs — stays English). Nothing here may be
// "translated"/ASCII-folded when copied elsewhere: these are byte-exact
// per the W12-06 task spec step 1.
package main

const (
	// MsgDaemonUnreachable is printed (with the dialed socket path
	// substituted via %s) whenever the CLI cannot complete a request against
	// kahyad - a dial failure, or (until W12-07 lands /v1/task) a 404 on a
	// route that doesn't exist yet. Exit code 2.
	MsgDaemonUnreachable = "kahyad'a ulaşılamıyor (%s). Başlatmak için: make install-agent"

	// MsgEmptyQuestion is printed when a one-shot prompt is empty/whitespace
	// after joining argv and trimming - rejected locally, before any dial.
	// Exit code 2.
	MsgEmptyQuestion = "Soru boş olamaz."

	// MsgREPLBanner is the first line the REPL prints on startup.
	MsgREPLBanner = "Kâhya hazır. Çıkmak için: /çık"

	// MsgREPLPrompt is printed (no trailing newline) before each REPL read.
	MsgREPLPrompt = "kâhya> "

	// MsgFarewell is printed once, right before the REPL exits (via /çık,
	// /cik, or EOF/Ctrl-D).
	MsgFarewell = "Görüşürüz."

	// MsgTraceFooter is printed to stderr after every one-shot/REPL answer
	// (with %s substituted by the trace_id), dim/faint when stderr is a
	// TTY. It is printed even on transport failure - the CLI mints the
	// trace_id itself precisely so it always has one to show.
	MsgTraceFooter = "iz: %s"

	// MsgLocallyProcessed is W3-08's CLI badge (task spec, byte-exact):
	// printed on its own stdout line whenever a task's terminal "result"
	// event carries processed_locally: true - the task was answered
	// entirely by the local secret-lane Qwen3-30B-A3B server, never a
	// cloud model.
	MsgLocallyProcessed = "🔒 yerel işlendi"

	// MsgLogNotFound is printed (with the queried trace_id substituted) when
	// `kahya log --trace <id>` finds zero matching JSONL lines. Exit 1.
	MsgLogNotFound = "Bu trace için kayıt bulunamadı: %s"

	// MsgTraceRequired is printed when `kahya log` is run without --trace.
	// Exit 2. Not one of the task spec's byte-exact strings (no --trace
	// flag was given, so there is no trace_id to fail gracefully around);
	// worded to match the register of the rest of this file.
	MsgTraceRequired = "log komutu için --trace gerekli."

	// MsgReindexSummary is `kahya reindex`'s success line: %d files_indexed,
	// %d chunks, %d duration_ms, in that order (matches the W12-04
	// POST /v1/reindex response fields).
	MsgReindexSummary = "Hafıza yeniden indekslendi: %d dosya, %d parça (%d ms)"

	// MsgHealthOK is `kahya health`'s success line: %d pid, %d schema
	// version, in that order.
	MsgHealthOK = "kahyad çalışıyor (pid %d, şema v%d)"

	// MsgHealthDegraded is printed (with the reported db status substituted)
	// when kahyad answers but reports its database as anything other than
	// "ok". Exit 1 (per W12-06 step 5: "nonzero exit if unreachable/degraded").
	MsgHealthDegraded = "kahyad çalışıyor ama hafıza veritabanı sorunlu (db=%s)"

	// MsgIdleTimeout is printed (with the trace_id substituted via %s) when
	// a /v1/task SSE stream goes 30s without a byte. Exit 2, same class as
	// MsgDaemonUnreachable (the task cannot be confirmed to have completed
	// from here - it may still be running server-side).
	MsgIdleTimeout = "kahyad yanıt vermiyor (30 sn) — görev arka planda sürüyor olabilir. Kontrol: kahya log --trace %s"

	// MsgTaskFailed is the fallback line printed when a terminal "result"
	// SSE event reports status="error" without its own optional "message"
	// field (MINOR 5) - so an error result is never silently swallowed with
	// no explanation on stderr.
	MsgTaskFailed = "Görev başarısız oldu."

	// MsgStreamIncomplete is printed (with the trace_id substituted via %s)
	// when a /v1/task SSE stream ends - cleanly or via a low-level read
	// error - after at least one byte had already arrived but before a
	// terminal "result"/"error" event (MINOR 6). Distinct from
	// MsgDaemonUnreachable: the daemon was reachable and the task may have
	// progressed or completed server-side, so the message points at
	// `kahya log` rather than implying the daemon is down.
	MsgStreamIncomplete = "Görev yanıtı yarıda kesildi, sonuç alınamadı. Kontrol: kahya log --trace %s"

	// ---- W3-02: kahya autonomy / kahya undo ----

	// MsgAutonomyUsage is printed when `kahya autonomy <bad-subcommand>` is
	// given anything other than no args or "promote". Exit 2.
	MsgAutonomyUsage = "kullanım: kahya autonomy | kahya autonomy promote <araç> <sınıf> <alan>"

	// MsgAutonomyEmpty is printed by `kahya autonomy` when the ladder has
	// no earned state at all yet (every tool is still at fresh L0).
	MsgAutonomyEmpty = "Henüz kazanılmış otonomi yok (her şey L0 - Gözlemci)."

	// MsgAutonomyRow is one `kahya autonomy` ladder-state line: tool,
	// class, scope, level, consecutive_approvals, in that order.
	MsgAutonomyRow = "%-20s %-4s %-20s L%d  (%d ardışık onay)"

	// MsgAutonomyPromoteUsage is printed when `kahya autonomy promote` is
	// given anything other than exactly 3 positional args. Exit 2.
	MsgAutonomyPromoteUsage = "kullanım: kahya autonomy promote <araç> <sınıf> <alan>"

	// MsgAutonomyPromoted is `kahya autonomy promote`'s success line: tool,
	// class, scope, new level, in that order.
	MsgAutonomyPromoted = "%s / %s / %s artık L%d seviyesinde."

	// MsgUndoTraceRequired is printed when `kahya undo` is run without
	// --trace. Exit 2.
	MsgUndoTraceRequired = "undo komutu için --trace gerekli."

	// MsgUndoTriggered is `kahya undo --trace <id>`'s success line (%s =
	// the tool whose undo window was triggered). Recipe EXECUTION itself
	// is delegated to the owning tool (W3-03) - this only reports that the
	// window closed and the demotion was recorded.
	MsgUndoTriggered = "Geri alma tetiklendi: %s. (Geri alma tarifi ilgili araç tarafından uygulanır.)"

	// ---- W3-06: kahya approvals / kahya approve <id> ----

	// MsgApprovalsEmpty is printed by `kahya approvals` when there is
	// nothing pending.
	MsgApprovalsEmpty = "Bekleyen onay yok."

	// MsgApproveUsage is printed when `kahya approve` is given anything
	// other than exactly one positional argument (the pending approval
	// id). Exit 2.
	MsgApproveUsage = "kullanım: kahya approve <id>"

	// PromptW1W2YesNo is `kahya approve <id>`'s W1/W2 decision prompt (this
	// task's spec: "[e]vet/[h]ayır").
	PromptW1W2YesNo = "Onaylıyor musunuz? [e]vet/[h]ayır: "

	// PromptW3Literal is `kahya approve <id>`'s W3 decision prompt -
	// EXACT, byte-for-byte, per this task's spec (HANDOFF §5 safety #5:
	// W3 accepts nothing but the literally typed word "onayla", never
	// "evet", never "y"). Do not reword or add/remove punctuation.
	PromptW3Literal = "Bu eylem geri alınamaz (W3). Devam etmek için 'onayla' yazın:"

	// MsgApprovalApproved is printed after a successful approve.
	MsgApprovalApproved = "Onaylandı."

	// MsgApprovalDenied is printed after a decline (W1/W2's [h]ayır, or
	// anything other than the literal "onayla" for W3) - the CLI always
	// still calls POST /policy/feedback kind=deny in this case, demoting
	// the ladder (W3-02), never silently doing nothing. Exit 1.
	MsgApprovalDenied = "Reddedildi."
)
