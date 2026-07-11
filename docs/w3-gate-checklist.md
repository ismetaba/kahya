# W3 acceptance gate — manual live-surface checklist (W3-10)

This is the **manual** companion to the five permanent, automated gate
tests in `tests/w3/gate_test.go` (`go test ./tests/w3/... -run TestGate`).
Those five tests exercise every clause of HANDOFF §6's W3 acceptance
sentence against fakes (a fake Telegram Bot API server, a mock Anthropic
upstream) — they need no live BotFather token and no cloud credential, and
they are what `make test`/CI actually run. This document is for the parts
that only make sense against the **real, running system**: the real
policy.yaml, the real `~/Library/Application Support/Kahya/brain.db`, and
— where noted — the real local Qwen3-30B-A3B model.

Run these against a `kahyad` started directly (`./bin/kahyad &`, or the
installed launchd agent) with the repo's own committed `policy.yaml` and
default paths — **not** the `tests/w3/fixtures/policy.yaml` the automated
gate uses.

---

## 1. Live W3 CLI `onayla` (mirrors Gate 2) — **DONE**, run live

Since `mail_send` (the only `class: W3` tool currently registered in
policy.yaml) has no MCP tool implementation behind it yet, this checklist
drives the same generic `POST /policy/check` → `kahya approve <id>`
sequence a real `mail_send` call would go through, exactly like Gate 2's
own test does — see that test's own doc comment in `tests/w3/gate_test.go`
for why this is the correct thing to drive directly.

**Commands run** (against a real `./bin/kahyad`, real socket at
`~/Library/Application Support/Kahya/kahyad.sock`, executed 2026-07-11):

```bash
SOCK="$HOME/Library/Application Support/Kahya/kahyad.sock"

# 1a. "evet" must be rejected (denied) — W3 accepts ONLY the literal word "onayla".
curl -s --unix-socket "$SOCK" -X POST http://kahyad/policy/check \
  -H 'Content-Type: application/json' \
  -d '{"trace_id":"trace-live-w3-onayla-1783776132","task_id":"task-live-w3","tool_name":"mail_send","tool_input":{"to":"birisi@ornek.com","body":"canli test govdesi"}}'
# -> {"decision":"needs_approval", ..., "pending_approval_id":"9800c8188125d0cb9c44f8b58c36f4bdcd7fec223e5ddd4311ba929154d3fe1b"}

printf 'evet\n' | ./bin/kahya approve 9800c8188125d0cb9c44f8b58c36f4bdcd7fec223e5ddd4311ba929154d3fe1b
# -> prints the byte-exact rendered payload, then "Reddedildi." — exit code 1

# 1b. "onayla" (a FRESH pending approval — the "evet" one above is already consumed) must succeed.
curl -s --unix-socket "$SOCK" -X POST http://kahyad/policy/check \
  -H 'Content-Type: application/json' \
  -d '{"trace_id":"trace-live-w3-onayla-ok-1783776144","task_id":"task-live-w3-ok","tool_name":"mail_send","tool_input":{"to":"ikinci@ornek.com","body":"canli onay testi"}}'
# -> pending_approval_id fd45af6cd8b0aa1441884a0401b63998262ac853acf9c500a4fd831ba503005b

printf 'onayla\n' | ./bin/kahya approve fd45af6cd8b0aa1441884a0401b63998262ac853acf9c500a4fd831ba503005b
# -> prints the payload, then "Onaylandı." — exit code 0
```

**Real ledger rows** (`sqlite3 "$HOME/Library/Application Support/Kahya/brain.db"`):

```sql
sqlite> SELECT trace_id, kind, payload FROM events
   ...> WHERE trace_id='trace-live-w3-onayla-1783776132' ORDER BY id;
trace-live-w3-onayla-1783776132|policy_decision|{"class":"W3","decision":"needs_approval","event":"policy_decision","level":0,"reason":"W3 sınıfı eylemler her zaman yazılı yerel onay gerektirir.","scope":"global","task_id":"task-live-w3","tool":"mail_send"}
trace-live-w3-onayla-1783776132|policy_feedback_denied|{"class":"W3","event":"policy_feedback_denied","scope":"global","tool":"mail_send"}
trace-live-w3-onayla-1783776132|demoted|{"class":"W3","event":"demoted","from_level":0,"reason":"denied","scope":"global","to_level":0,"tool":"mail_send"}

sqlite> SELECT trace_id, kind, payload FROM events
   ...> WHERE trace_id='trace-live-w3-onayla-ok-1783776144' ORDER BY id;
trace-live-w3-onayla-ok-1783776144|policy_decision|{"class":"W3","decision":"needs_approval","event":"policy_decision","level":0,"reason":"W3 sınıfı eylemler her zaman yazılı yerel onay gerektirir.","scope":"global","task_id":"task-live-w3-ok","tool":"mail_send"}
trace-live-w3-onayla-ok-1783776144|policy_feedback_approved|{"class":"W3","consecutive_approvals":1,"event":"policy_feedback_approved","scope":"global","surface":"local","tool":"mail_send"}
```

Confirms live: "evet" never approves a W3 action (denies + demotes instead);
"onayla" is the only string that does, and the approved event carries
`surface:"local"` with no `"remote"` key.

---

## 2. Live secret-lane demo — 🔒 `yerel işlendi` badge + `pgrep mlx_lm.server` — **DONE**, run live

**Prerequisite verified present on this machine:** the pinned
`mlx-community/Qwen3-30B-A3B-4bit` snapshot
(`d388dead1515f5e085ef7a0431dd8fadf0886c57`) under
`~/.cache/huggingface/hub/`, and `mlx/qwen/.venv` with `mlx_lm` installed.

**Command run** (2026-07-11, real `./bin/kahya`, default config/paths):

```bash
./bin/kahya "IBAN TR33 0006 1005 1978 6457 8413 26 için ödeme talimatı hakkında kısa bir özet ver"
```

**Output** (elided model prose, badge byte-exact):

```
TR33 0006 1005 1978 6457 8413 26 IBAN numarasına yapılan ödemeler, ... [local model's own answer text] ...
🔒 yerel işlendi
iz: 4671de6de19974a1caadaab47461106c
```

**`pgrep` confirms the local model process, not a cloud call:**

```bash
$ pgrep -fl mlx_lm.server
27942 .../mlx/qwen/.venv/bin/python -m mlx_lm.server --model /Users/matt/.cache/huggingface/hub/models--mlx-community--Qwen3-30B-A3B-4bit/snapshots/d388dead1515f5e085ef7a0431dd8fadf0886c57 --host 127.0.0.1 --port 8765
```

**Real ledger + task row** (`trace_id=4671de6de19974a1caadaab47461106c`):

```sql
sqlite> SELECT trace_id, kind, payload FROM events WHERE trace_id='4671de6de19974a1caadaab47461106c' ORDER BY id;
4671de6de19974a1caadaab47461106c|sensitive_read_marked|{"event":"sensitive_read_marked","session_id":"4671de6de19974a1caadaab47461106c","task_id":""}
4671de6de19974a1caadaab47461106c|task_spawned|{"lane":"secret","model":"claude-sonnet-5","task_id":"t_744126a3f49259ced65f1f31ec606f2d"}
4671de6de19974a1caadaab47461106c|task_done|{"status":"ok","task_id":"t_744126a3f49259ced65f1f31ec606f2d"}

sqlite> SELECT id, trace_id, lane, secret_category, state FROM tasks WHERE trace_id='4671de6de19974a1caadaab47461106c';
t_744126a3f49259ced65f1f31ec606f2d|4671de6de19974a1caadaab47461106c|secret|finans|done
```

`lane='secret'`, `secret_category='finans'` — the deterministic IBAN
pre-pass classified it correctly; the answer came entirely from the local
model (`sensitive_read_marked` + `task_done` with no worker/proxy ever
spawned for this task — task.go's own "no envelope, no worker/proxy for a
secret-lane task to exist at all" framing).

The daemon and the local model process were stopped cleanly afterward
(`SIGINT` → `shutdown_complete` logged, `mlx_lm.server` reaped as part of
`qwenSup.Stop()`) — this checklist does not leave a live-loaded 30B model
running on the machine.

---

## 3. Live Telegram W2 card approve (mirrors Gate 1) — **DEFERRED**

**Blocked on:** a real BotFather token (`kahya.telegram` Keychain item,
W0-04/W3-07) and the user DMing the bot once to learn the real
`chat_id`/`user_id` allowlist pair. Neither exists yet in this
environment. `tests/w3/gate_test.go`'s Gate 1 already exercises the exact
same code path end-to-end against a fake Telegram Bot API server (byte-exact
diff, inline keyboard, approve-via-callback, single-trace_id ledger chain)
— this live step is for confirming the *real* Telegram apps/servers behave
identically, once the token exists.

**Exact commands to run once the token is available:**

```bash
# 1. Provision the token (one time).
security add-generic-password -s kahya.telegram -a kahya -T "$(which kahyad)" -w
#   <paste the BotFather token when prompted>

# 2. DM the bot once from your phone/Telegram app (any message), then read
#    the update back to learn chat_id/user_id:
curl -s "https://api.telegram.org/bot<TOKEN>/getUpdates" | python3 -m json.tool
#   -> note .result[0].message.chat.id and .result[0].message.from.id

# 3. Add the pair to config.yaml (~/Library/Application Support/Kahya/config.yaml):
#    telegram_chat_id: <chat.id>
#    telegram_user_id: <from.id>

# 4. Restart kahyad (`make install-agent` + `launchctl kickstart -k gui/$(id -u)/com.kahya.kahyad`,
#    or `./bin/kahyad &` for a quick manual check), then trigger a real W2
#    approval (fs_write is class W1 in the committed policy.yaml — use
#    `kahya` to ask it to write a file, or POST /policy/check directly for
#    fs_write/fs_delete):
./bin/kahya "günlüğüme şunu ekle: bugün ilk Telegram onay testini yaptım"
#    -> kahyad sends a Telegram card with the byte-exact diff + Onayla/Reddet
#       buttons to your phone. Tap Onayla.

# 5. Confirm the ledger:
sqlite3 "$HOME/Library/Application Support/Kahya/brain.db" \
  "SELECT trace_id, kind, payload FROM events WHERE trace_id='<the iz: trace_id printed>' ORDER BY id;"
#    -> expect policy_decision(needs_approval) -> policy_feedback_approved
#       (surface:"telegram", remote:true) -> fs_write, all one trace_id.
```

Record the real trace_id + ledger output here once run.
