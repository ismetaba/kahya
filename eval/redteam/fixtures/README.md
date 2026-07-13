# W78-02 red-team fixtures — record-replay for offline determinism

The red-team eval must run with **no network, no real cloud key, no off-box
byte** (HANDOFF §6 W7-8 ⚑: "egress deny-all + record-replay SDK fixture'ları").
This directory holds the record-replay pieces the dev worker would be spawned
against when a scenario needs a worker/SDK interaction:

- `replay_server.py` — a tiny local HTTP server that speaks the Anthropic
  Messages API shape and serves a canned response from `transcripts/`. The dev
  worker is spawned with `ANTHROPIC_BASE_URL=http://127.0.0.1:<port>` (the same
  forward-proxy mechanism kahyad's per-task proxy uses), so a worker run is
  deterministic and offline.
- `transcripts/` — recorded, **synthetic** attacker transcripts. These are
  never real user memory; each is a hand-authored adversarial payload matching
  a `scenarios/*.yaml` attack.

## Why the four shipped scenarios don't spawn a worker

The four required scenarios (`scenarios/01..04`) attack the **enforcement
plane directly** — factengine's tier gate, the egress gate, the WYSIWYE
approval-token hash, and taint persistence. Those are Go-level boundaries that
sit *below* the worker, so the harness (`kahyad/internal/eval/redteam.go`)
exercises the real enforcement packages in-process with byte-exact adversarial
inputs — strictly stronger and more deterministic than routing the same bytes
through a mocked worker. The replay server here is the substrate for **future**
scenarios (W78-03 collection) that need a full worker round-trip; it is kept so
`make eval-redteam` needs no network even when such a scenario is added.

## Running

`make eval-redteam` sets `KAHYA_ENV=dev` and runs `kahya eval redteam`, which
runs the four scenarios against the dev-profile brain.db and (in the live
drill) records the counts/hashes-only `eval.redteam.result` summary row in the
production ledger. The hermetic `go test` gate
(`kahyad/internal/eval` `TestRedteamAllScenariosBlocked`) runs the same four
scenarios with no daemon and no network.
