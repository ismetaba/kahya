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

	// ---- W4-02: kahya task show <id> / kahya task resolve <id> ----

	// MsgTaskUsage is printed when `kahya task` is given anything other
	// than a recognized "show"/"resolve" subcommand. Exit 2.
	MsgTaskUsage = "kullanım: kahya task show <id> | kahya task resolve <id> --retry|--abort"

	// MsgTaskShowUsage is printed when `kahya task show` is given anything
	// other than exactly one positional argument (the task id). Exit 2.
	MsgTaskShowUsage = "kullanım: kahya task show <id>"

	// MsgTaskResolveUsage is printed when `kahya task resolve` is missing
	// the task id, or given neither (or both) of --retry/--abort. Exit 2.
	MsgTaskResolveUsage = "kullanım: kahya task resolve <id> --retry|--abort"

	// MsgTaskShowHeader/Status/Session/PID/Attempts/ToolCallsHeader/
	// ToolCallRow together render `kahya task show <id>`'s output, one
	// line each.
	MsgTaskShowHeader        = "Görev %s"
	MsgTaskShowStatus        = "Durum: %s"
	MsgTaskShowSession       = "Oturum: %s"
	MsgTaskShowPID           = "Aktif işlemci (PID): %s"
	MsgTaskShowAttempts      = "Deneme sayısı: %d"
	MsgTaskShowToolCallsNone = "Araç çağrısı yok."
	MsgTaskShowToolCallsHead = "Araç çağrıları:"
	MsgTaskShowToolCallRow   = "  #%d %s [%s] durum=%s"

	// MsgTaskShowNone fills MsgTaskShowSession/MsgTaskShowPID when there is
	// no session_id/no live worker PID.
	MsgTaskShowNone = "yok"

	// MsgTaskResolvedRetry/Abort are `kahya task resolve <id>`'s success
	// lines.
	MsgTaskResolvedRetry = "Görev yeniden kuyruğa alındı: %s"
	MsgTaskResolvedAbort = "Görev durduruldu (failed): %s"

	// ---- W4-05: kahya ledger verify ----

	// MsgLedgerUsage is printed when `kahya ledger` is given anything other
	// than the recognized "verify" subcommand. Exit 2.
	MsgLedgerUsage = "kullanım: kahya ledger verify"

	// MsgLedgerVerifyOK is `kahya ledger verify`'s success line: the
	// from-genesis recompute agreed with every anchored checkpoint (and,
	// when anchor.remote is configured, with the remote's own anchors.log
	// too) - no tampering detected. Exit 0.
	MsgLedgerVerifyOK = "Defter doğrulandı: kurcalama tespit edilmedi."

	// ---- W5-01: kahya job run <name> (extends the W4-01 kahya-trigger
	// mechanism with a subcommand on the main CLI) ----

	// MsgJobUsage is printed when `kahya job` is given anything other than
	// the recognized "run <name>" subcommand. Exit 2.
	MsgJobUsage = "kullanım: kahya job run <ad>"

	// MsgJobTriggered is `kahya job run <name>`'s success line: %s = the
	// job name, %s = the trace_id this run was assigned (so the user can
	// follow up with `kahya log --trace <id>`).
	MsgJobTriggered = "İş tetiklendi: %s (iz: %s)"

	// ---- W5-02: kahya consolidation show|approve|reject ----

	// MsgConsolidationUsage is printed when `kahya consolidation` is given
	// anything other than a recognized "show"/"approve"/"reject"
	// subcommand. Exit 2.
	MsgConsolidationUsage = "kullanım: kahya consolidation show|approve|reject"

	// MsgConsolidationEmpty is printed by `kahya consolidation show` (and
	// approve/reject) when there is no pending suggestion at all.
	MsgConsolidationEmpty = "Bekleyen konsolidasyon önerisi yok."

	// PromptConsolidationApprove is `kahya consolidation approve`'s
	// decision prompt - literal-only, mirroring PromptW3Literal's own
	// "onayla" gate (this is a W1 memory-write, not W3, but the task spec
	// names the SAME literal confirm word for this specific action).
	PromptConsolidationApprove = "Konsolidasyon önerisini ana dala birleştirmek istiyor musunuz? Devam etmek için 'onayla' yazın:"

	// MsgConsolidationApproved/Rejected are the two terminal outcomes.
	MsgConsolidationApproved = "Konsolidasyon onaylandı."
	MsgConsolidationRejected = "Konsolidasyon reddedildi."

	// ---- W5-04: kahya fact confirm|retract, kahya entity merge|split ----

	// MsgFactUsage is printed when `kahya fact` is given anything other
	// than a recognized "confirm"/"retract" subcommand. Exit 2.
	MsgFactUsage = "kullanım: kahya fact confirm <id> | kahya fact retract <özne> <yüklem> <nesne> [oturum_id]"

	// MsgFactConfirmed is `kahya fact confirm <id>`'s success line: %d =
	// the fact id.
	MsgFactConfirmed = "Olgu onaylandı: %d"

	// MsgFactRetracted is `kahya fact retract ...`'s success line: %d =
	// the retracted fact's id.
	MsgFactRetracted = "Olgu geri çekildi: %d"

	// MsgEntityUsage is printed when `kahya entity` is given anything
	// other than a recognized "merge"/"split" subcommand. Exit 2.
	MsgEntityUsage = "kullanım: kahya entity merge <a> <b> --evidence <olgu_id> | kahya entity split <birleştirme_defteri_id>"

	// MsgEntityMergeUsage is printed when `kahya entity merge` is missing
	// its two entity-id positional args or its required --evidence flag.
	MsgEntityMergeUsage = "kullanım: kahya entity merge <a> <b> --evidence <olgu_id>"

	// MsgEntityMerged is `kahya entity merge`'s success line: %d = the
	// new merge_ledger row's id (needed later for `kahya entity split`).
	MsgEntityMerged = "Varlıklar birleştirildi (birleştirme defteri kaydı: %d)."

	// MsgEntitySplitUsage is printed when `kahya entity split` is missing
	// its merge_ledger id positional arg.
	MsgEntitySplitUsage = "kullanım: kahya entity split <birleştirme_defteri_id>"

	// MsgEntitySplit is `kahya entity split`'s success line: %d = the new
	// split merge_ledger row's id.
	MsgEntitySplit = "Varlıklar ayrıldı (birleştirme defteri kaydı: %d)."

	// ---- W5-03: kahya remembered --trace <id> ----

	// MsgRememberedUsage is printed when `kahya remembered` is given
	// anything other than exactly the --trace flag. Exit 2.
	MsgRememberedUsage = "kullanım: kahya remembered --trace <id>"

	// MsgRememberedTraceRequired is printed when `kahya remembered` is run
	// without --trace. Exit 2.
	MsgRememberedTraceRequired = "remembered komutu için --trace gerekli."

	// MsgRememberedSaved is `kahya remembered --trace <id>`'s success
	// line - byte-exact per the W5-03 task spec (identical to the
	// Telegram "🌟 Hatırladı" button's own success toast,
	// kahyad/internal/telegram's toastRememberedSaved).
	MsgRememberedSaved = "🌟 Hatırladı anı kaydedildi."
)
