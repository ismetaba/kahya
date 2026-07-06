# W0-01 — Memory repo seed

**Status:** todo
**Phase:** W0 — Day-1 setup
**Depends on:** none
**Flags:** user-assist
**Handoff refs:** §7, §3

## Goal

`~/Kahya` exists as its own git repository (separate from the code repo) with `memory/` seeded
from the user's existing memory corpus plus gold-token project notes, a first commit, the
one-time 10-minute user review completed, and a private remote configured. This is the
cold-start antidote: W12-04 indexes this corpus and the W1–2 acceptance gate retrieves from it.

## Context you need

HANDOFF §7 (binding, quote verbatim):

> ⚑ **Dizin adları ASCII** (`~/Kahya`) — non-ASCII `â` policy.yaml globlarında ve Docker/SQLite bayt-düzeyi karşılaştırmalarında NFC/NFD sessiz uyuşmazlık riski taşır; "Kâhya" yalnızca ürün/görünen ad. ⚑ **Kod ayrı repoda** (`~/code/kahya`), `~/Kahya` yalnız hafıza git'i (konsolidasyon commit'leri kod geçmişine karışmasın).

> ⚑ **Tohum tier eşlemesi:** tohum dosyaları içe aktarımda kullanıcı **10 dakikalık tek seferlik gözden geçirmeden** geçirir (yanlış/bayat notları siler); sağ çıkan olgular `source_tier=user_asserted` (≤.95) alır — gerekçe: kullanıcının kendi oturumlarında biriktirip fiilen sahiplendiği notlar, gözden geçirme karantinayı kaldıran kullanıcı onayı sayılır. Böylece §5-Hafıza-#1 karantina kuralı bozulmadan W1–2 kabul kriteri (tohumdan `<hafiza>` enjeksiyonu) çalışır.

HANDOFF §3: seed sources are `~/.claude/projects/-Users-matt-Test/memory/` (11 project note
files + `MEMORY.md` index — **the index is excluded**, only real memory files are seeded) and
`~/Project 1/gold-token` — "**hafızayı gün 1'de bununla tohumla** (soğuk-başlangıç, terk
edilmenin 1 numaralı nedeni)". gold-token README/notes provide person/project/topology context
for devops scenarios.

Gotchas:
- The source path `~/Project 1/gold-token` contains a space — quote it in every command.
- Memory files are Turkish; copy bytes as-is (`rsync -a`), never re-encode or rename.
- `source_tier` is a brain.db column that does not exist yet; W0-01 only records that the
  review happened (repo-root README) so W12-04/W5-04 can apply `user_asserted` at extraction.

## Deliverables

- `~/Kahya/` — new git repo (branch `main`) containing `memory/` (seeded corpus, including
  flat `memory/gold-token-*.md` project notes — no subdirectories, see step 4),
  `backups/.gitkeep`, and a root `README.md` (provenance + review record).
  No file named `memory/MEMORY.md`.
- Git history: seed commit, post-review commit; remote `origin` = user's **private** remote
  (privacy verified — the corpus is personal data).
- No files created or modified in `~/code/kahya`, except the task-protocol bookkeeping
  required by tasks/README.md (this task file's `Status:` line + the BACKLOG.md checkbox,
  committed as `[W0-01] …`). No seeded memory content enters the code repo.

## Steps

1. Preflight: `[ ! -e ~/Kahya ] || [ -z "$(ls -A ~/Kahya)" ]` must exit 0 (do not clobber an
   existing repo — if it fails, STOP and ask the user); confirm
   `ls ~/.claude/projects/-Users-matt-Test/memory/*.md` lists ≥10 files.
2. Create and init (ASCII dir name, deterministic branch):
   ```bash
   mkdir -p ~/Kahya/memory ~/Kahya/backups && cd ~/Kahya && git init -b main
   touch backups/.gitkeep
   ```
3. Seed the corpus exactly as HANDOFF §7 specifies (index excluded):
   ```bash
   rsync -a --exclude='MEMORY.md' \
     ~/.claude/projects/-Users-matt-Test/memory/ ~/Kahya/memory/
   ```
4. Seed gold-token context as FLAT files directly under `memory/` — the memory source-of-truth
   glob is `~/Kahya/memory/*.md` (HANDOFF §9; W12-04 indexes exactly this glob) and it is NOT
   recursive: a `memory/gold-token/` subdirectory would silently never be indexed, defeating
   §3's "hafızayı gün 1'de bununla tohumla":
   ```bash
   cp "$HOME/Project 1/gold-token/README.md"              ~/Kahya/memory/gold-token-README.md
   cp "$HOME/Project 1/gold-token/backend/README.md"      ~/Kahya/memory/gold-token-backend-README.md
   cp "$HOME/Project 1/gold-token/docs/system-design.md"  ~/Kahya/memory/gold-token-system-design.md
   ```
   (Skip any source file that does not exist; do not copy code.)
5. Write `~/Kahya/README.md` (root, NOT under `memory/` so W12-04 never indexes it as memory):
   state that this repo is the memory source of truth (Markdown + git, HANDOFF §4/§9), list the
   two seed sources, and leave a line `Gözden geçirme: BEKLİYOR` to be updated in step 7
   (byte-exact Turkish — dotted `İ`, never ASCII-fold, per tasks/README.md language rule).
6. First commit, message verbatim from HANDOFF §7:
   ```bash
   cd ~/Kahya && git add -A && git commit -m "seed: import existing memory corpus"
   ```
7. **USER (10 minutes, one-time):** ask the user (in Turkish) to review every file under
   `~/Kahya/memory/` and delete/fix wrong or stale notes. Per the ⚑ tier mapping above, this
   review is the user approval that lifts §5-Memory-#1 quarantine: surviving facts will be
   extracted as `source_tier=user_asserted` (≤.95). Update the README line to
   `Gözden geçirme: TAMAM (<YYYY-MM-DD>) — sağ çıkan olgular source_tier=user_asserted (HANDOFF §7)`.
   Commit: `git add -A && git commit -m "seed: post-review prune (user review complete)"`.
8. **USER:** obtain the private remote URL (empty private repo, e.g. GitHub private), then:
   ```bash
   cd ~/Kahya && git remote add origin <private-remote-url> && git push -u origin main
   ```
   Verify the remote is **private** before pushing — the corpus is personal data and may
   contain secret-lane-adjacent (finans/sağlık/kimlik) notes: for a `github.com` remote run
   `gh repo view <owner/repo> --json isPrivate -q .isPrivate` (must print `true`); for any
   other host, have the user confirm and record the line
   `Remote: private (kullanıcı onayı, <YYYY-MM-DD>)` in the root README before `git push`.
9. If the user is unavailable for step 7 or 8: finish everything else, set this file's
   `Status: blocked-user`, mark `[!]` in BACKLOG.md with the exact missing input
   ("10-dk tohum gözden geçirmesi" and/or "özel git remote URL'i").

## Acceptance criteria

- [ ] `git -C ~/Kahya log --oneline | grep -c 'seed: import existing memory corpus'` prints 1.
- [ ] `test ! -e ~/Kahya/memory/MEMORY.md` exits 0 (index excluded).
- [ ] `ls ~/Kahya/memory/*.md | wc -l` ≥ 10 (seeded corpus present).
- [ ] `test -f ~/Kahya/memory/gold-token-README.md` exits 0 and
      `find ~/Kahya/memory -mindepth 2 -name '*.md' | wc -l` prints 0 (all seed notes are flat
      files caught by the `~/Kahya/memory/*.md` glob that W12-04 indexes).
- [ ] `grep -l 'ev' ~/Kahya/memory/*.md` is non-empty (a seed note containing `ev` exists —
      prerequisite for the W12-10 `'evlerimizden'` trigram acceptance query).
- [ ] `test -d ~/Kahya/backups` exits 0.
- [ ] Review done: `grep 'Gözden geçirme: TAMAM' ~/Kahya/README.md` matches and a post-review
      commit exists after the seed commit (`git -C ~/Kahya rev-list --count HEAD` ≥ 2) —
      or Status is `blocked-user` naming this.
- [ ] Remote works: `git -C ~/Kahya remote get-url origin` prints a URL and
      `git -C ~/Kahya push --dry-run` exits 0 — or Status is `blocked-user` naming this.
- [ ] Remote privacy verified: `gh repo view <owner/repo> --json isPrivate -q .isPrivate`
      prints `true` (github.com remotes), or `grep 'Remote: private' ~/Kahya/README.md`
      matches (other hosts, user-confirmed) — or Status is `blocked-user` naming this.
- [ ] `git -C ~/code/kahya status --porcelain` shows no changes from this task other than
      the protocol bookkeeping (this task file + `tasks/BACKLOG.md`); in particular
      `git -C ~/code/kahya grep -l 'gold-token' -- ':!tasks' ':!docs'` finds nothing — no
      memory content leaked into the code repo.

## Out of scope

- Indexing into brain.db, episodes/chunks, FTS5 (W12-02/W12-03/W12-04) — this task is files+git only.
- Fact extraction, `source_tier` column mechanics, confidence (W12-02, W5-04).
- Nightly `git push` automation and backup rotation (W4-06); `backups/` git policy is W4-06's call.
- Consolidation / commit-discipline (`author=kahyad`) logic (W5-02).
- SwiftUI memory browser — deferred by HANDOFF §8 (memory = markdown, open with an editor).
