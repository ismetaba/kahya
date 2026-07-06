# W0-05 — Design doc snapshot

**Status:** todo
**Phase:** W0 — Day-1 setup
**Depends on:** none
**Flags:** user-assist
**Handoff refs:** §7, §9

## Goal

The full design artifact (architecture diagram, scenarios, tables — the superset of
HANDOFF.md) is committed into the repo as `docs/design.html`, so the design survives even if
the claude.ai artifact link dies. After this task the repo is self-sufficient as a design
source: HANDOFF.md (locked decisions) + design.html (full detail).

## Context you need

HANDOFF preamble (binding, quote verbatim):

> Tam tasarım dokümanı (mimari diyagram, senaryolar, tablolar): https://claude.ai/code/artifact/466f3b05-4443-4ba9-ab8c-02ee17ab315f — **ve** Gün 1'de repoya `docs/design.html` olarak commit edilir (link ölürse belge kendine yetmeli).

HANDOFF §7 repo skeleton:

> #   docs/design.html   (tam tasarım artifact'ının kopyası — §9 linki buna güncellenir)

HANDOFF §9:

> **Tam tasarım (diyagram + senaryolar + tablolar):** https://claude.ai/code/artifact/466f3b05-4443-4ba9-ab8c-02ee17ab315f · repoda `docs/design.html`.

Note on "§9 linki buna güncellenir": §9 ALREADY says "repoda `docs/design.html`", so no edit to
HANDOFF.md is required (HANDOFF.md is the locked design doc — do not modify it).

Gotchas:
- claude.ai artifacts sit behind the user's login; an unauthenticated `curl` will usually
  return a login shell page or an error, NOT the artifact. Detect this instead of committing
  garbage (that is the 🧍 reason).
- "belge kendine yetmeli" ⇒ the saved HTML must be self-contained: content readable offline,
  no required external scripts/stylesheets. Artifacts are built self-contained (inline
  CSS/JS), so a faithful export passes this.

## Deliverables

- `/Users/matt/code/kahya/docs/design.html` — the complete artifact, committed.
- Nothing else changes: `docs/HANDOFF.md` and `README.md` untouched; the only other edits are
  the task-protocol bookkeeping required by tasks/README.md (this task file's `Status:` line +
  the BACKLOG.md checkbox).

## Steps

1. Attempt an automated fetch:
   ```bash
   curl -fsSL -o /Users/matt/code/kahya/docs/design.html \
     'https://claude.ai/code/artifact/466f3b05-4443-4ba9-ab8c-02ee17ab315f'
   ```
   (A WebFetch tool, if available, may be used instead — same URL, same validation.)
2. Validate the fetched file (ALL must hold, else it is a login page/error, delete it):
   - `wc -c` ≥ 50000 bytes (the design doc is large; a login shell is not),
   - `grep -Eqi 'K[âa]hya' docs/design.html` matches,
   - `grep -Eqi 'otonomi' docs/design.html` matches (design-specific content, not chrome).
3. If validation fails: delete the bad file and ask the **USER** (in Turkish):
   "Tasarım artifact'ını tarayıcıda aç (HANDOFF §9'daki link), sayfayı tam HTML olarak dışa
   aktar/kaydet ve `~/code/kahya/docs/design.html` olarak bırak." Set `Status: blocked-user`,
   mark `[!]` in BACKLOG.md with that one-liner, and stop here until the file appears. When
   the user delivers the file, re-run step 2's validation on it.
4. Self-containedness check (belge kendine yetmeli):
   ```bash
   ! grep -Eq '<(script|link|img|iframe)[^>]*(src|href|srcset)="https?://' /Users/matt/code/kahya/docs/design.html
   ```
   must exit 0 (no external scripts, stylesheets, images, or frames — the offline render must
   be complete, not just script-free). If it fails, ask the user for a
   fully self-contained export (e.g. the artifact's own download, or "Webpage, Complete" then
   inline) — do not hand-edit design content.
5. Eyeball render: `open /Users/matt/code/kahya/docs/design.html` — the page must show the
   design document (architecture diagram, scenario sections, tables), not a login or error page.
6. Commit: `[W0-05] snapshot full design artifact as docs/design.html`.

## Acceptance criteria

- [ ] `test -s /Users/matt/code/kahya/docs/design.html` exits 0 and
      `wc -c < docs/design.html` ≥ 50000.
- [ ] `grep -Eqi 'K[âa]hya' docs/design.html && grep -Eqi 'otonomi' docs/design.html` exits 0.
- [ ] Self-contained: the step-4 grep for external `script`/`link`/`img`/`iframe`
      `src`/`href`/`srcset` references finds nothing.
- [ ] Full-document markers present (not just a title/login page):
      `grep -Eqi 'senaryo' docs/design.html && grep -Eqi 'kahyad' docs/design.html` exits 0
      (scenario sections + architecture content). Step 5's browser render is advisory; these
      greps are the gate.
- [ ] `git -C /Users/matt/code/kahya log --oneline -1 -- docs/design.html` shows the
      `[W0-05]` commit; `git show --stat` for that commit touches ONLY `docs/design.html`
      plus the task-protocol bookkeeping (this task file, `tasks/BACKLOG.md`).
- [ ] HANDOFF.md locked and untouched:
      `git -C /Users/matt/code/kahya log --oneline -- docs/HANDOFF.md | grep -c 'W0-05'`
      prints 0 and `git -C /Users/matt/code/kahya diff --quiet HEAD -- docs/HANDOFF.md` exits 0.

## Out of scope

- Editing/regenerating/translating the design content, or "fixing" it to match HANDOFF.md —
  if they conflict, HANDOFF.md wins by protocol (tasks/README.md), and the snapshot stays a
  faithful copy.
- Modifying HANDOFF.md §9 or any link inside it (locked document).
- Converting to Markdown/PDF, adding a docs site, or any doc tooling.
- `docs/coverage.md` (W78-03), `docs/restore-runbook.md` (W78-05), `docs/dogfood.md` (W78-06),
  `docs/models.md` (W0-03).
