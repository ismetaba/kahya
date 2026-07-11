# mlx/embed — local Qwen3-Embedding-0.6B service (W12-11)

Small stdlib `http.server` HTTP service serving the pinned
`Qwen/Qwen3-Embedding-0.6B` checkpoint over MLX. `kahyad` supervises this
process (`kahyad/internal/mlxsup`) exactly like it will later supervise
the Qwen3-30B-A3B secret-lane server (W3-08) — launchd only ever holds
`kahyad` itself (HANDOFF §4 ⚑).

Own venv (`mlx/embed/.venv`), own pinned `requirements.txt`, separate from
`worker/`'s deps (HANDOFF §4: three-process architecture — kahyad / Agent
SDK worker / MLX helper — each with its own runtime).

## Why not `mlx_lm.server`?

`mlx_lm.server` is OpenAI-chat-compatible but has no `/v1/embeddings`
endpoint. This file follows the exact same supervision contract (127.0.0.1
only, fixed config port, no auth, kahyad health-checks it) but is a
purpose-built embedding server instead.

## API

```
GET /health
  -> {"status": "ok", "model_ver": "qwen3-embedding-0.6b:512:v1"}

POST /v1/embeddings
  {"model": "qwen3-embedding-0.6b", "input": ["text one", "text two", ...]}
  -> {
       "object": "list",
       "data": [
         {"object": "embedding", "index": 0, "embedding": [0.01, ...512 floats]},
         {"object": "embedding", "index": 1, "embedding": [...]}
       ],
       "model": "qwen3-embedding-0.6b",
       "usage": {"prompt_tokens": N, "total_tokens": N}
     }
```

Binds `127.0.0.1:$KAHYA_EMBED_PORT` (default 8092) **only** — never
`0.0.0.0`. Batches are capped at 64 inputs per request (larger batches get
a `400`; callers chunk themselves — `kahyad/internal/embed`'s backfill
already batches at 32).

## Embedding recipe (Qwen3-Embedding model card)

1. Tokenize the raw input text (no special prefix added server-side —
   callers decide whether to prepend an instruction prefix to a query).
2. Run the decoder-only transformer (`mlx_lm.models.qwen3.Qwen3Model`,
   loaded directly rather than through `mlx_lm.utils.load`'s outer
   causal-LM wrapper — this checkpoint's safetensors keys have no
   `model.` prefix and no `lm_head`, since it's an embedding checkpoint,
   not a generation one).
3. **Last-token pooling**: take the final sequence position's hidden
   state, after the model's own final RMSNorm.
4. **L2-normalize** the full 1024-dim vector.
5. **Truncate to 512 dims** (Matryoshka/MRL — the model card documents
   support for output dims from 32 to 1024) and **L2-normalize again**
   (truncating a unit vector does not leave it unit-norm).

One sequence at a time, no padding: `Qwen3Model`'s attention mask has no
notion of a padding mask, so naively batching different-length sequences
together would let real tokens attend into padding-token embeddings.
Running unpadded, one at a time, sidesteps this entirely — measured at
roughly 7ms/embed once warmed up on an M-series Mac, well within budget
at kahyad's corpus scale (HANDOFF §4: "≤~100k chunks").

Deterministic by construction: no sampling, no dropout at inference — the
same input always produces the bit-identical output (`test_server.py`
asserts this).

## Setup

```
cd mlx/embed
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

The model itself is **not** downloaded here — W0-03 already pulled
`Qwen/Qwen3-Embedding-0.6B` @ `97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3`
into the default Hugging Face cache (`~/.cache/huggingface/hub`).
`server.py` loads it with `local_files_only=True`: a missing/incomplete
local snapshot makes startup fail loudly (surfaced by
`kahyad/internal/mlxsup` as a crash + backoff-restart loop), it never
silently re-downloads.

`kahyad` starts this process **lazily** — on the first search that needs
the vector leg, not at boot (HANDOFF §6 timing note) — via
`cfg.embed_cmd` (default
`["mlx/embed/.venv/bin/python", "mlx/embed/server.py"]`, repo-root
relative, resolved the same way `worker_cmd` is). You do not need to run
this file by hand for normal use; `make test-mlx` and manual debugging are
the two reasons to.

## Manual run / debug

```
KAHYA_EMBED_PORT=8092 mlx/embed/.venv/bin/python mlx/embed/server.py
curl -s 127.0.0.1:8092/health
curl -s 127.0.0.1:8092/v1/embeddings \
  -d '{"model":"qwen3-embedding-0.6b","input":["merhaba dunya"]}'
```

## Version-switch procedure (model upgrade)

Per HANDOFF §4 ⚑ model_ver rule: **mixed-version KNN is forbidden** — a
KNN query is always filtered to the single active `model_ver`
(`kahyad/internal/search.Searcher.vecLeg`). Switching the active embedding
model/revision is therefore always a **full re-embed from the Markdown
source of truth**, never an in-place upgrade:

1. **Before switching**: the §5-Memory-#5 retrieval QA eval gate must be
   green for the candidate model/revision. This gate does not exist yet
   (it lands with W78-01) — until then, this is a **mandatory manual
   step**: do not flip `active_embed_model_ver` without a human first
   confirming retrieval quality didn't regress. Do **not** build eval-gate
   automation here; that is explicitly W78-01's job.
2. Update `MODEL_REPO` / `MODEL_REVISION` in `server.py` (and re-download
   the new snapshot via W0-03's mechanism, or `huggingface_hub`
   directly) if the model itself is changing; bump `MODEL_VER` to a new
   string (e.g. `qwen3-embedding-0.6b:512:v2`, or a wholly different model
   name) — this string is what tags every future `chunk_vec` row.
3. Set the new `active_embed_model_ver` in kahyad's `config.yaml`.
4. Restart kahyad (`make install-agent` re-bootstrap, or `launchctl
   kickstart`).
5. Run `bin/kahya reindex --re-embed` (`POST /v1/reindex
   {"full":false,"re_embed":true}`): every chunk gets re-embedded under
   the new active `model_ver`, then every `chunk_vec` row still tagged
   with any OTHER `model_ver` is deleted.
6. Verify: `sqlite3 brain.db "SELECT count(DISTINCT model_ver) FROM
   chunk_vec;"` → `1`, and it matches the new `active_embed_model_ver`.

A chunk whose re-embed attempt fails mid-run (embed service down, etc.) is
left vector-less rather than mixed-version — FTS still serves it
(degraded, never broken) — and is retried on the next reindex.
