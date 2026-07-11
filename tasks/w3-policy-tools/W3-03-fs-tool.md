# W3-03 — fs MCP tool (kahyad-owned) with deny globs + undo recipes

**Status:** done
**Phase:** W3 — Policy + tools
**Depends on:** W3-02
**Flags:** none
**Handoff refs:** §5 safety #6 ⚑ deny globs, §4 ladder (undo), §5 safety #5 (canonical paths), §7 TCC checklist

## Goal
A kahyad-owned fs MCP tool exposing `fs_read`, `fs_write`, `fs_delete`. Writes/deletes are classified W1, enforce the policy.yaml write-deny globs on canonicalized paths, consume a one-time approval token when the ladder requires it, and implement the two undo recipes (delete→Trash, write→pre-op git checkpoint) that back the W1 5-minute undo window.

## Context you need
The binding deny globs (HANDOFF §5 safety #6):

> ⚑ **`fs` yazma-deny globları (policy.yaml, Gün 1):** `~/.zshrc` ve shell rc/profil dosyaları, `~/Library/LaunchAgents/**`, `~/.hammerspoon/**`, `~/Library/Application Support/Kahya/**` (defter/DB'nin kendi kendini kurcalamasına karşı).

Undo recipes (HANDOFF §4 ladder): "policy.yaml araç kaydında `reversible: true/false` + araç-başına undo tarifi (silme→Çöp, dosya yazımı→işlem-öncesi git checkpoint, mail→taslak-asla-gönderme)". Canonicalization matters because WYSIWYE (§5 #5) requires "kanonik yol/host" — a deny glob must not be bypassable via `~/Library/LaunchAgents/../LaunchAgents/x`, symlinks, or NFD-encoded path segments.

Architecture: per §5 enforcement plane, side-effectful MCP servers are either spawned/owned by kahyad or verify one-time tokens. This tool lives **in kahyad's process as a Go MCP server** (same pattern as `mcp/memory/` from W12-05); the worker reaches it only through kahyad. It still calls `POST /policy/consume-token` before any write — defense in depth, and it keeps a single code path when tools later run out-of-process.

Prior outputs: W3-01 loaded globs; W3-02 provides check/consume-token/undo-window APIs and `kahya undo --trace <id>`. Note from §7: an fs tool reading protected dirs needs Full Disk Access granted to the *responsible process* under launchd — do not chase TCC failures in this task; log them cleanly (full TCC checklist work is W6-01's checklist pattern).

Gotchas:
- macOS default filesystem (APFS) is case-insensitive but case-preserving: glob matching must be case-insensitive too, or `~/library/launchagents/x` slips through. Canonicalize case via the on-disk name where the path exists; lowercase-compare for deny-glob matching.
- APFS stores names NFD-ish at some layers; that is why canonicalization NFC-normalizes the *string* before glob matching, while filesystem ops use the original resolved path.
- `os.Rename` into `~/.Trash` fails cross-device (`EXDEV`) for external volumes — detect and fall back to copy+remove into `~/.Trash`, still never plain `unlink`.
- Deny-glob check runs BEFORE approval flow: an approval can never be minted for a denied path, so a confused user cannot "approve" self-tampering.

## Deliverables
- `mcp/fs/server.go`, `mcp/fs/paths.go`, `mcp/fs/undo.go` (Go package registered into kahyad's MCP server set).
- Tool registrations wired to policy.yaml entries `fs_read` (R), `fs_write` (W1), `fs_delete` (W1).
- Undo implementations: Trash restore + git-checkpoint restore, invoked by kahyad when `kahya undo --trace <id>` fires inside the window.
- Tests: `mcp/fs/paths_test.go`, `mcp/fs/server_test.go`, `mcp/fs/undo_test.go`.

## Steps
1. `paths.go` — canonicalization used by every operation: expand `~`, `filepath.Abs`, resolve symlinks with `filepath.EvalSymlinks` on the deepest existing ancestor, NFC-normalize the string (`golang.org/x/text/unicode/norm`), reject paths containing bidi/zero-width code points. Deny-glob matching runs on the canonical result only.
2. `fs_read`: class R; goes through `/policy/check` like everything else (R is auto-allowed from L1 per ladder); returns bytes + metadata. On `EPERM` from TCC, return a structured error whose user-facing message is Turkish: `Tam Disk Erişimi gerekli: <path>`. **Secret-lane read marking (seam for W3-05):** match the canonical path against `secret_lane_globs` (loaded by W3-01); on match, emit ledger event `secret_lane_read` with `trace_id` + `session_id` and set `secret_lane: true` in the tool's result metadata. W3-05 wires this seam to its `POST /session/sensitive-read` session flag (the §5 #1 "hassas okuma" trigger); until then the ledger event is the durable record.
3. `fs_write`: before touching the file — (a) canonicalize; (b) if canonical path matches any `fs_write_deny_globs` ⇒ DENY immediately, ledger event `fs_deny_glob_hit`, no approval can override; (c) consume approval token (W3-02) when policy said NEEDS_APPROVAL→approved, or verify ALLOW; (d) **pre-op git checkpoint (§4 locked undo recipe: "dosya yazımı→işlem-öncesi git checkpoint")**: if the target is inside a git work tree, checkpoint the pre-image into that repo's object database with `git hash-object -w <file>` (no working-tree or index side effects) and record the blob SHA in the undo record + ledger event; if the target is NOT inside any git work tree, fall back to copying the pre-image to `~/Library/Application Support/Kahya/undo/<task_id>/<sha256-of-canonical-path>`. Either way the undo record stores `{canonical_path, git_blob_sha | copy_path, pre_hash}`; (e) write atomically (temp file + rename); (f) ledger event with pre/post content hashes.
4. `fs_delete`: same gate chain; recipe = move to Trash via `os.Rename` into `~/.Trash/` with collision-safe suffix (record original path in the ledger event payload); never `unlink` directly.
5. `undo.go`: kahyad-invoked handlers — `undo_write` restores the pre-image (from the git blob via `git cat-file blob <sha>` or from the fallback copy, per the undo record); `undo_delete` moves the file back from Trash to the recorded original path. Both write ledger events `undo_executed` and report to `/policy/feedback` (which demotes per W3-02).
6. Purge fallback pre-image copies when the 5-minute window expires (hook the `undo_window_expired` event) — keep `~/Library/Application Support/Kahya/undo/` bounded. Git checkpoint blobs need no purge (unreferenced blobs are reclaimed by the repo's own `git gc`).
7. Tests: deny-glob bypass attempts MUST fail — fixtures: `~/Library/LaunchAgents/../LaunchAgents/evil.plist`, a symlink `~/tmp/lnk → ~/.zshrc`, NFD-encoded `~/Library/Application Support/Kahya/…` variant; write→undo→byte-identical restore; delete→undo→file back at original path; write without a valid token fails when ladder says NEEDS_APPROVAL.

## Acceptance criteria
- [x] `go test ./mcp/fs/...` green in `make test`, including all three bypass fixtures (dotdot, symlink, NFD).
- [x] Manual: with kahyad running, an `fs_write` to `~/.zshrc` requested through the worker is denied; JSONL log line with `"event":"fs_deny_glob_hit"` and the request's `trace_id` exists; `~/.zshrc` mtime unchanged.
- [x] Manual: `fs_write` to a scratch file at L2 auto-runs; `kahya undo --trace <id>` within 5 minutes restores the byte-exact pre-image (compare `shasum -a 256` before/after); ledger shows `undo_executed`.
- [x] `fs_delete` moves the file to `~/.Trash/` (verify with `ls`), never unlinks — test asserts the Trash entry exists.
- [x] Token replay test: reusing a consumed token on a second `fs_write` fails (covered in tests, runs in `make test`).
- [x] Undo pre-images purged after window expiry: after the 5-minute window lapses in a test (inject a short window via config), `~/Library/Application Support/Kahya/undo/<task_id>/` is empty.
- [x] Case-insensitivity test green: `fs_write` to `~/LIBRARY/LaunchAgents/x.plist` is denied like its lowercase form.
- [x] `fs_read` of a path matching a `secret_lane_globs` fixture (e.g. `~/Documents/saglik/tahlil-sonuçları.pdf`, byte-exact Turkish path) produces a `secret_lane_read` ledger event with `trace_id` and `secret_lane: true` in the result metadata (test) — W3-05 consumes this seam.
- [x] Every fs operation logs one JSONL line with `trace_id` and canonical path (grep a manual run's log).

## Out of scope
- Shell execution of any kind — W3-04 (and the binary allowlist is NOT a security boundary, §5 #6).
- Egress (this tool never sends bytes off-box) — W3-05.
- WYSIWYE diff rendering for the approval surfaces — W3-06 (this task supplies canonical paths/hashes to it).
- Watching/indexing `~/Kahya/memory` — W12-04 owns indexing; memory writes go through `memory_write`, not `fs_write`.
- Full Disk Access / TCC grant automation — user-assist, handled with the W6-01 checklist.
- Endpoint Security file monitoring — deferred (§8).
