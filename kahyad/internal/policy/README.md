# kahyad/internal/policy — W3-02 wire schema

`POST /policy/check` `{tool_name, tool_input, scope?, task_id, trace_id}` → `{decision: "allow"|"needs_approval"|"deny", reason?, rule, pending_approval_id?, token?}`. `class` is never client-supplied — resolved from the loaded `policy.yaml`. `token` appears only on `allow` for a non-R (side-effectful) class; `pending_approval_id` only on `needs_approval`.

`pending_approval_id` (post-security-review amendment) is a server-issued, single-use, DB-tracked reference: `Engine.Check` inserts a `pending_approvals` row (32 random bytes hex id, bound to the RESOLVED tool/class/scope/task_id/trace_id/approved_bytes_hash, 10-minute TTL) and returns its id — it is opaque and carries no caller-decodable meaning. `Engine.Approve`/`Deny` look the row up by id and atomically single-use-consume it (`consumed_at IS NULL`); a forged, expired, or already-consumed id is rejected outright, before any token is minted or any bookkeeping runs.

`POST /policy/consume-token` `{token, tool, class, scope, task_id, trace_id, tool_input}` → `{ok, error?}`. Side-effectful tools MUST call this, with the EXACT bytes they are about to execute, immediately before executing. Single-use; any failure demotes the token's REAL bound `(tool,class,scope)` — persisted on `approval_tokens` at mint time and recovered by `token_hash` — never the `tool`/`class`/`scope` this request body happens to claim, and ledgers `token_verify_failed`. A hash matching no token row at all demotes nothing (there is no real triple to punish) but still ledgers.

`POST /policy/feedback` `{kind: "approve"|"deny"|"undo", pending_approval_id?, surface?, trace_id?}` → `{ok, token?, error?}`. `approve` mints a token (W3 requires `surface:"local"`, checked against the pending_approvals row's real class — a rejected non-local W3 approval attempt does NOT consume the id, so it stays usable for a later local approval); `deny`/`undo` demote.

`GET /policy/state` → `{states: [{tool, class, scope, level, consecutive_approvals, updated_at}]}`.

`POST /policy/promote` `{tool, class, scope}` → `{level}` — the only path that ever raises a level (`kahya autonomy promote`).

`POST /policy/undo` `{trace_id}` → `{ok, tool, task_id}` — triggers an open undo window; recipe execution is the owning tool's job (W3-03).
