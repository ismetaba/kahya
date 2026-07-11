# kahyad/internal/policy — W3-02 wire schema

`POST /policy/check` `{tool_name, tool_input, scope?, task_id, trace_id}` → `{decision: "allow"|"needs_approval"|"deny", reason?, rule, pending_approval_id?, token?}`. `class` is never client-supplied — resolved from the loaded `policy.yaml`. `token` appears only on `allow` for a non-R (side-effectful) class; `pending_approval_id` only on `needs_approval`.

`POST /policy/consume-token` `{token, tool, class, scope, task_id, trace_id, tool_input}` → `{ok, error?}`. Side-effectful tools MUST call this, with the EXACT bytes they are about to execute, immediately before executing. Single-use; any failure demotes `(tool,class,scope)` and ledgers `token_verify_failed`.

`POST /policy/feedback` `{kind: "approve"|"deny"|"undo", pending_approval_id?, surface?, trace_id?}` → `{ok, token?, error?}`. `approve` mints a token (W3 requires `surface:"local"`); `deny`/`undo` demote.

`GET /policy/state` → `{states: [{tool, class, scope, level, consecutive_approvals, updated_at}]}`.

`POST /policy/promote` `{tool, class, scope}` → `{level}` — the only path that ever raises a level (`kahya autonomy promote`).

`POST /policy/undo` `{trace_id}` → `{ok, tool, task_id}` — triggers an open undo window; recipe execution is the owning tool's job (W3-03).
