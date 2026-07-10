# W12-06 — kahya CLI

**Status:** done
**Phase:** W1–2 — Core
**Depends on:** W12-01
**Flags:** none
**Handoff refs:** §4 UI

## Goal
The user's primary interaction surface for W1–5: a `kahya` binary with one-shot mode, a REPL, `kahya log --trace <id>`, plus `health` and `reindex` subcommands — all talking to kahyad over the UDS with Turkish user-facing strings.

## Context you need
The UI decision (HANDOFF §4, verbatim):

> - **UI (MVP):** **`kahya` CLI** (tek-atış + REPL + `kahya log --trace <id>`; daemon'a UDS üzerinden konuşur — **W1–5'in birincil etkileşim yüzeyi budur, palet W6'da gelir**) + Hammerspoon (global kısayol + onay kartı + `⌥⎋` acil durdurma) + **Telegram botu** (uzaktan onay, yalnız geri-alınabilir eylemler). **SwiftUI menü-çubuğu app v2 işi** — MVP'de yok.

Language policy (HANDOFF §3): "**Dil politikası:** Sohbet/UI Türkçe-öncelikli; teknik çıktı (kod, log, model ID) İngilizce." — CLI strings Turkish; flags, subcommand names, log/JSON keys English. The display name may use `â` ("Kâhya" yalnızca ürün/görünen ad, §7 ⚑) but no path the CLI touches may be non-ASCII.

Location per §7 skeleton: `kahyad/cmd/kahya/` — it can import `kahyad/internal/...` (config, traceid). Use stdlib `flag` + `os.Args` subcommand dispatch; HANDOFF names no CLI framework, so don't add one.

Task execution transport: the CLI POSTs to kahyad and streams the answer. Until W12-07/W12-09 land, `POST /v1/task` doesn't exist yet — build the CLI against the endpoint contract below, keep `ask`/REPL failing gracefully with the Turkish daemon-error string, and land `health`/`log`/`reindex` fully working now. Define the contract here so W12-07 implements the server side to match:
- `POST /v1/task` body `{"prompt": "...", "trace_id": "..."}` (CLI mints the trace_id so it can print it even on transport failure).
- Response: `text/event-stream`; events `delta` (`{"text":"..."}`), `result` (`{"status":"ok|error","task_id":"...","session_id":"..."}`), `error` (`{"message":"..."}` — message is user-facing Turkish).

## Deliverables
- `kahyad/cmd/kahya/main.go` — dispatch: no args → REPL; first arg not a subcommand → one-shot ask; subcommands `log`, `health`, `reindex`.
- `kahyad/cmd/kahya/client.go` — UDS HTTP client (`http.Transport` with `DialContext` to the unix socket; socket from `KAHYA_SOCKET` env else default path), SSE reader.
- `kahyad/cmd/kahya/strings.go` — ALL Turkish strings as named constants (single reviewable file).
- `kahyad/cmd/kahya/main_test.go` + `client_test.go` — tests against a fake UDS server.
- Makefile: `bin/kahya` added to `make build`.
- kahyad side: `GET /v1/log?trace_id=<id>` — reads `<log_dir>/*.jsonl`, returns all lines whose `trace_id` matches, ordered by `ts` (implemented in kahyad in this task; it is read-only log plumbing, not task logic).

## Steps
1. Turkish strings (byte-exact, in `strings.go`):
   - daemon unreachable: `kahyad'a ulaşılamıyor (%s). Başlatmak için: make install-agent` (exit code 2)
   - empty question: `Soru boş olamaz.` (exit 2)
   - REPL banner: `Kâhya hazır. Çıkmak için: /çık` · prompt: `kâhya> ` · farewell: `Görüşürüz.`
   - trace footer after each answer: `iz: %s` (dim/faint if TTY)
   - `log` with no records: `Bu trace için kayıt bulunamadı: %s` (exit 1)
   - reindex result: `Hafıza yeniden indekslendi: %d dosya, %d parça (%d ms)`
   - health OK: `kahyad çalışıyor (pid %d, şema v%d)`
2. One-shot: `kahya "soru..."` — join remaining args as the prompt, mint trace_id, POST `/v1/task`, stream `delta` text to stdout as it arrives, then print `iz: <trace_id>` to stderr. Exit 0 on `result.status=="ok"`, 1 on `error`.
3. REPL: read lines (stdlib `bufio.Scanner`); `/çık` (also accept `/cik`, EOF/Ctrl-D) exits; each line = one task with a fresh trace_id; print the trace footer after each answer. No history/readline dependency for MVP.
4. `kahya log --trace <id>`: GET `/v1/log`, pretty-print each JSONL line as `HH:MM:SS.mmm  LEVEL  [proc]  event  key=val…` (proc derived from source file name, e.g. `kahyad`/`worker`); `--raw` flag dumps raw JSONL.
5. `kahya health`: GET `/health`, print the Turkish health line; nonzero exit if unreachable/degraded.
6. `kahya reindex`: POST `/v1/reindex` `{}`, print the Turkish summary (W12-04 endpoint; `--full` flag maps to `{"full":true}`).
7. Every request sets `X-Kahya-Trace-Id`. Client timeouts: connect 2s; no overall deadline on the SSE stream (long tasks), but 30s idle-read timeout with Turkish timeout error `kahyad yanıt vermiyor (30 sn) — görev arka planda sürüyor olabilir. Kontrol: kahya log --trace %s`.
8. Tests: fake UDS server fixtures — SSE happy path assembles deltas in order; daemon-down prints the exact unreachable string and exits 2; `log --trace` renders fixture JSONL (fixture includes a Turkish payload string `Kadıköy randevusu` verified byte-exact end-to-end); empty prompt rejected locally without dialing.

## Acceptance criteria
- [ ] `make build` produces `bin/kahya`; `make test` green including CLI package tests.
- [ ] With kahyad stopped: `bin/kahya "merhaba"` prints the exact unreachable string (with socket path substituted) and exits 2.
- [ ] With kahyad running (W12-04 done): `bin/kahya reindex` prints `Hafıza yeniden indekslendi: …` with real counts; `bin/kahya health` prints `kahyad çalışıyor …` and exits 0.
- [ ] `bin/kahya log --trace <id>` for a trace_id taken from `kahyad.jsonl` prints ≥1 formatted line; for a bogus id prints `Bu trace için kayıt bulunamadı: …` and exits 1.
- [ ] REPL: `printf '/çık\n' | bin/kahya` prints banner then `Görüşürüz.` and exits 0.
- [ ] After W12-09 lands: `bin/kahya "…"` streams an answer and prints `iz: <trace_id>` (re-verified in W12-10 — note this criterion in the PR but do not block this task on it).

## Out of scope
- The `/v1/task` server-side implementation (spawn, streaming) — W12-07/W12-09. This task only defines the contract and implements the client.
- Approval prompts / written `onayla` input — W3 (WYSIWYE + policy); the REPL has no approval UX yet.
- Hammerspoon palette, `⌥Space`, `⌥⎋` — W6-01/W6-03. Voice — W6-02.
- `kahya metrics` — W78-04. Shell completions, colors beyond faint trace footer, readline history — not MVP.
