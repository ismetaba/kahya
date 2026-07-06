# Kâhya

**Kâhya** is a local-first, Turkish-first personal AI assistant for a single power user, built on three pillars:

1. **Living Memory** — everything told to or observed by the assistant is stored as a user-owned, human-editable corpus (`~/Kahya/memory/*.md`, git) with a derived SQLite index, and improves over years.
2. **Helm of the Mac** — operates the user's real Mac (mail, terminal, files, apps) on their behalf, under earned autonomy, undo, and an append-only ledger.
3. **Proactivity** — initiates conversations itself via schedulers and watchers (morning briefing, "CI is red", "your passport expires soon").

The metaphor: an Ottoman **kâhya** — runs the household *on its own* in the owner's name, earning trust over years. The entire safety architecture rests on this.

## Read this first

| Document | Purpose |
|---|---|
| [`docs/HANDOFF.md`](docs/HANDOFF.md) | **The locked design document.** Architecture, locked decisions (§4), safety/memory invariants (§5), 8-week MVP critical path (§6), day-1 kickoff (§7). Source of truth for everything. |
| [`tasks/README.md`](tasks/README.md) | How to work the backlog: task-picking protocol, conventions, hard rules, task file template. |
| [`tasks/BACKLOG.md`](tasks/BACKLOG.md) | Ordered index of all build tasks with dependencies and status. |
| `docs/design.html` | Full design artifact copy (committed by task W0-05). |

## Repo layout

```
kahyad/            Go daemon — control plane (policy, ledger, scheduler, SQLite index, Keychain)
kahyad/cmd/kahya/  Go CLI — one-shot + REPL + `kahya log --trace <id>`, talks to daemon over UDS
worker/            Python — claude-agent-sdk reasoning harness, spawned per-task by kahyad
mcp/memory/        Go (compiled into kahyad) — memory_search / memory_write / memory_forget
docs/              Handoff + design artifact + generated coverage map
tasks/             The complete MVP build plan, executable task by task
policy.yaml        Action classes, reversibility, secret-lane globs, egress allowlist (created in W3-01)
```

Related directories **outside** this repo (per HANDOFF §7/§9 — code and memory are deliberately separate git repos):

- `~/Kahya/memory/*.md` — memory source of truth (own git repo, private remote)
- `~/Kahya/backups/` — nightly `VACUUM INTO` copies of brain.db
- `~/Library/Application Support/Kahya/brain.db` — derived SQLite index + ledger (WAL)
- macOS Keychain — `kahya.anthropic` / `kahya.telegram` / `kahya.anchor`

## Status

Greenfield. Design is complete and locked. Next: execute `tasks/BACKLOG.md` top to bottom.

**MVP is done when:** 2 weeks of uninterrupted daily use · zero data-loss events (one rehearsed backup restore) · ≥10 commands/day · ≥5 "it remembered" moments/week · egress / secret-lane / W3 invariants covered by code tests, green in CI.
