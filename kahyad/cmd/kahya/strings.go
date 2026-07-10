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
)
