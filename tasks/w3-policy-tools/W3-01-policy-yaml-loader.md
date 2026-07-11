# W3-01 — policy.yaml schema + validating loader

**Status:** done
**Phase:** W3 — Policy + tools
**Depends on:** W12-01
**Flags:** none
**Handoff refs:** §5 safety #1/#6 ⚑, §4 ladder (action classes + undo), §4 routing ⚑ ordering invariant

## Goal
A committed `policy.yaml` at the repo root plus a strict, validating Go loader in kahyad. After this task, every tool registration, action-class label, undo recipe, secret-lane glob, fs write-deny glob, and egress allowlist entry has exactly one machine-readable source of truth that kahyad loads (and fail-closes on) at startup.

## Context you need
Action classes (HANDOFF §4, defined before first use):

> - **R** = salt-okuma · **W1** = geri-alınabilir yazma · **W2** = sert/zor geri alınır yazma · **W3** = geri dönüşsüz (para · prod · kimlik · senin adına mesaj)

Undo metadata lives on the tool registration (HANDOFF §4):

> - **Geri-alma (undo):** policy.yaml araç kaydında `reversible: true/false` + araç-başına undo tarifi (silme→Çöp, dosya yazımı→işlem-öncesi git checkpoint, mail→taslak-asla-gönderme). "Geri-alınabilir" sınıflandırması Telegram onay kapısını (§5 #5) ve W1 5-dk penceresini besler.

Mandatory fs write-deny globs (HANDOFF §5 safety #6):

> ⚑ **`fs` yazma-deny globları (policy.yaml, Gün 1):** `~/.zshrc` ve shell rc/profil dosyaları, `~/Library/LaunchAgents/**`, `~/.hammerspoon/**`, `~/Library/Application Support/Kahya/**` (defter/DB'nin kendi kendini kurcalamasına karşı).

Secret-lane globs cover file paths ONLY (HANDOFF §4):

> ⚑ **Sıralama değişmezi:** *Hiçbir bayt, gizli-şerit sınıflandırması yerel/deterministik olarak tamamlanmadan bulut modele gitmez.* policy.yaml globları **yalnız dosya yolları** için; mail/web gibi içerik-kaynaklı veride gizli-şerit kararı yerel içerik-sınıflandırıcıyla **alım anında** verilir.

Egress is a gated capability whose allowlist + volume budgets live here (HANDOFF §5 safety #1): "Off-box'a byte gönderen her çağrı (HTTP gövde *ve* URL, DNS, mail, panoya-uzak) hedef **allowlist** + hacim bütçesine tabi." Enforcement is W3-05; this task only defines and validates the config.

Prior output you build on: W12-01 gives the kahyad skeleton, config loading, JSONL logging with `trace_id`. Global convention (tasks/README.md): any policy error ⇒ DENY, never a permissive fallback.

## Deliverables
- `policy.yaml` (repo root, committed) — initial real content per Steps 2–4.
- `kahyad/internal/policy/schema.go` — typed structs for the whole document.
- `kahyad/internal/policy/loader.go` — parse + validate + normalize (glob expansion of `~`).
- `kahyad/internal/policy/loader_test.go` — table-driven validation tests incl. Turkish-path fixtures.
- kahyad startup wiring: load at boot; on any validation error enter **deny-all mode** (every future `/policy/check` returns DENY) and log `{"level":"error","event":"policy_load_failed",...}`.

## Steps
1. Define the YAML schema in `schema.go`:
   - `tools:` list — each entry: `name`, `class` (one of `R|W1|W2|W3`), `reversible` (bool), `undo` (free-form recipe string, required when `reversible: true`), `scope_key` (string template naming the ladder scope dimension, e.g. `fs.top_dir`, `egress.host`; default `"global"`).
   - `secret_lane_globs:` — file-path globs only (e.g. `~/Kahya/memory/finans*.md`, `~/Documents/saglik/**`, `~/Documents/kimlik/**`). Add a schema comment: content-sourced data (mail/web) is classified at ingest by W3-08, never by these globs.
   - `fs_write_deny_globs:` — MUST contain at minimum: `~/.zshrc`, `~/.zprofile`, `~/.zshenv`, `~/.bashrc`, `~/.bash_profile`, `~/.profile`, `~/Library/LaunchAgents/**`, `~/.hammerspoon/**`, `~/Library/Application Support/Kahya/**`. Loader rejects a policy.yaml missing any of these (hard validation error — they are Day-1 invariants, not preferences).
   - `egress:` — `allowlist:` entries (`host`, optional `ports`, optional `methods`), `default_daily_byte_budget`, optional per-host `daily_byte_budget` overrides. Budget values are user-tunable; ship defaults of `26214400` (25 MiB) per host.
2. Write initial `policy.yaml` registering the tools known to the MVP: `memory_search` (R), `memory_write` (W1, reversible: true, undo: `git revert of the memory commit`), `memory_forget` (W1, reversible: true, undo: `git revert`), `fs_read` (R), `fs_write` (W1, reversible: true, undo: `pre-op git checkpoint restore`), `fs_delete` (W1, reversible: true, undo: `move back from Trash`), `shell_docker` (W2, reversible: false), `shell_host` (W2, reversible: false), `applescript_run` (W2, reversible: false), `jxa_run` (W2, reversible: false), `shortcuts_run` (W2, reversible: false) — the exact tool names W3-09 implements; its loader-enforced "never below W2" floor is added there — `mail_draft` (W1, reversible: true, undo: `taslak-asla-gönderme — draft is never sent, delete draft`), `mail_send` (W3, reversible: false), `telegram_send` (W2, reversible: false).
3. Fill `egress.allowlist` with the MVP's known targets only: `api.anthropic.com`, `api.telegram.org`, `api.github.com`, `github.com`, plus the user's private memory-git remote host (leave a placeholder comment; W0-01 supplies it) and the W4-05 external ledger-anchor remote host (second placeholder comment; W4-05 supplies it — its anchor push must pass `egress.Check`, so the host must be addable without a schema change).
4. Add `secret_lane_globs` seed values (step 1 examples) and the full `fs_write_deny_globs` set.
5. Implement `loader.go`: strict YAML (unknown keys = error), class enum check, `reversible: true` requires non-empty `undo`, `W3` entries must have `reversible: false` (irreversible by definition), glob syntax check (compile each with `github.com/bmatcuk/doublestar/v4`, pinned in go.mod — stdlib `path.Match` cannot express the `**` that the mandatory deny globs require, so it is not an option), `~` expanded to the real home dir at load time (paths stay ASCII per §7).
6. Wire into kahyad startup before the UDS listener accepts `/policy/check`; implement deny-all mode flag consumed by the (interim W12-07, later W3-02) policy handler.
7. Tests: valid fixture loads; each validation rule has a failing fixture; a fixture with `fs_write_deny_globs` missing `~/Library/Application Support/Kahya/**` fails; glob matching works on paths containing Turkish characters (fixture path `~/Documents/saglik/tahlil-sonuçları.pdf` matches `~/Documents/saglik/**` byte-exactly, no ASCII folding).

## Acceptance criteria
- [x] `go test ./kahyad/internal/policy/...` green and included in `make test`.
- [x] `policy.yaml` at repo root parses: a `kahyad policy validate` subcommand (add it) exits 0 and prints the tool count; `kahyad policy validate` against a fixture missing a mandatory deny glob exits non-zero.
- [x] Start kahyad with a deliberately broken `policy.yaml`: JSONL log contains `"event":"policy_load_failed"` and a subsequent `POST /policy/check` over the UDS returns DENY (deny-all mode) — verify with `curl --unix-socket ~/Library/Application\ Support/Kahya/kahyad.sock`.
- [x] Test proves W3-class tool with `reversible: true` is rejected at load.
- [x] All four mandatory deny-glob families (§5 #6 quote above: shell rc/profile files; `~/Library/LaunchAgents/**`; `~/.hammerspoon/**`; `~/Library/Application Support/Kahya/**`) present in committed `policy.yaml` — checked by a test, not by eyeball.

## Out of scope
- Enforcing any of this (ladder decisions, deny-glob checks, egress blocking) — W3-02/W3-03/W3-05.
- Content-based secret-lane classification (mail/web) — W3-08; the globs here are file-path-only by design.
- Autonomy-level persistence/promotion — W3-02.
- Per-tool undo *execution* — W3-03.
- SQLCipher, Endpoint Security, or any §8 deferred mechanism.
