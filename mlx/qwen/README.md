# mlx/qwen — local Qwen3-30B-A3B secret-lane server (W3-08)

`kahyad`-supervised `mlx_lm.server` (a third-party CLI, part of the pinned
`mlx-lm` package below) serving the pinned `mlx-community/Qwen3-30B-A3B-4bit`
checkpoint (docs/models.md: revision `d388dead1515f5e085ef7a0431dd8fadf0886c57`,
already downloaded by W0-03 into the default Hugging Face cache — this
directory does not re-download anything). `kahyad/internal/mlx.Supervisor`
reuses `kahyad/internal/mlxsup`'s generic spawn/health-poll/crash-restart
engine verbatim (see that package's own doc comment) — this is NOT a second
supervisor implementation.

Own venv (`mlx/qwen/.venv`), own pinned `requirements.txt`, separate from
`worker/`'s and `mlx/embed/`'s deps (HANDOFF §4: three-process architecture
— kahyad / Agent SDK worker / MLX helper — plus each MLX helper server gets
its own venv).

## Why `mlx_lm.server` (unlike `mlx/embed/server.py`)?

`mlx_lm.server` is already OpenAI-chat-compatible (`GET /v1/models`,
`POST /v1/chat/completions`) — exactly the API shape
`kahyad/internal/secretlane`'s classifier and local answerer need. Unlike
the embedding service (W12-11, which needed a bespoke server because
`mlx_lm.server` has no `/v1/embeddings` endpoint), there is nothing to
build here beyond installing the pinned package and pointing it at the
downloaded model.

## Setup

```bash
cd mlx/qwen
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

## Running standalone (manual verification, outside kahyad)

```bash
mlx/qwen/.venv/bin/mlx_lm.server \
  --model ~/.cache/huggingface/hub/models--mlx-community--Qwen3-30B-A3B-4bit/snapshots/d388dead1515f5e085ef7a0431dd8fadf0886c57 \
  --host 127.0.0.1 --port 8765
```

Cold load streams ~16GB from disk — expect 1-2 minutes before
`GET http://127.0.0.1:8765/v1/models` answers. `kahyad` never runs this
manually; `kahyad/internal/mlx.Supervisor` spawns it lazily (config
`mlx.qwen_cmd`/`mlx.qwen_model_path`/`mlx.qwen_port`, defaults in
`kahyad/internal/config/config.go`) on the first secret-lane request, and
unloads it (SIGTERM+reap) after `mlx.qwen_idle_ttl_seconds` (default 600 =
10 min) of zero in-flight requests.

## `requirements.txt` note

`transformers` is pinned to `5.8.0` deliberately (matching `mlx/embed/
requirements.txt`'s own identical pin and its documented reason): newer
5.x releases broke `mlx-lm==0.31.3`'s import at module load with
`AttributeError: 'str' object has no attribute '__module__'` — 5.8.0 is
the newest verified-working release as of this pin. `mlx`/`mlx-lm` are
pinned to the exact same versions `mlx/embed/requirements.txt` uses
(`mlx==0.32.0`, `mlx-lm==0.31.3`).

## Live verification (`KAHYA_MLX_TESTS=1`)

`make test`'s ordinary run never loads this ~16GB model (every
deterministic/fail-closed/proxy-backstop path in `kahyad/internal/mlx` and
`kahyad/internal/secretlane` is unit-tested against fakes, with no MLX
dependency at all). To exercise a REAL spawn + health-check + classify +
idle-unload against the real server:

```bash
KAHYA_MLX_TESTS=1 go test ./kahyad/internal/mlx/... -run Live -v -timeout 10m
```
