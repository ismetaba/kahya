#!/usr/bin/env python3
"""mlx/embed/server.py -- kahyad-supervised local MLX embedding service
(W12-11; HANDOFF S4 model fleet + supervision contract).

Serves Qwen3-Embedding-0.6B (pinned revision below) over a small stdlib
http.server HTTP API, OpenAI-shaped, following the SAME supervision
contract as mlx_lm.server (127.0.0.1 only, fixed config port, no auth):
mlx_lm.server itself has no /v1/embeddings endpoint, which is why this
service exists as a small helper of our own rather than reusing it
directly (see the task spec's implementation note).

Endpoints:
    GET  /health         -> {"status":"ok","model_ver":"<MODEL_VER>"}
    POST /v1/embeddings   {"model":"qwen3-embedding-0.6b","input":["...",...]}
                         -> {"object":"list","data":[{"object":"embedding",
                             "index":0,"embedding":[...512 floats...]},...],
                             "model":"qwen3-embedding-0.6b",
                             "usage":{"prompt_tokens":N,"total_tokens":N}}

Embedding recipe (Qwen3-Embedding model card, followed exactly):
    1. Tokenize (no manual instruction prefix added here - the CALLER
       decides whether to prepend an instruct prefix to a query string;
       this service embeds whatever text it is given, verbatim).
    2. Run the decoder-only transformer, causal attention, ONE sequence
       at a time (no padding - see _embed_one's own doc comment for why).
    3. LAST-TOKEN pooling: take the final position's hidden state after
       the model's own final RMSNorm.
    4. L2-normalize the full 1024-dim vector.
    5. Truncate to the first 512 dims (Matryoshka/MRL - Qwen3-Embedding
       is trained to support this) and L2-normalize AGAIN (truncating an
       already-unit vector does not itself have unit norm).

Deterministic by construction: no sampling, no dropout at inference, a
fixed pinned revision - identical input always produces identical output
(bit-for-bit, module load state held constant across calls) - see
test_server.py's determinism assertion.

Model loading: this file loads Qwen3Model (the embedding checkpoint's own
architecture - see the ModelArgs field list below, taken directly from
its config.json) via mlx-lm's Qwen3 model code, but bypasses mlx_lm.load's
own high-level convenience wrapper. Reason: the Qwen3-Embedding-0.6B HF
checkpoint's safetensors keys have NO "model." prefix and no lm_head
(tied embeddings, and the lm_head is never needed for embedding output at
all) - they map directly onto mlx_lm.models.qwen3.Qwen3Model's own
sub-modules (embed_tokens/layers/norm), not onto the outer causal-LM
Model wrapper mlx_lm.utils.load expects. Loading Qwen3Model directly
avoids fighting that mismatch and is both simpler and more obviously
correct than working around it.
"""

from __future__ import annotations

import http.server
import json
import os
import sys
import threading
from dataclasses import dataclass

import mlx.core as mx
from huggingface_hub import snapshot_download
from mlx_lm.models.qwen3 import ModelArgs, Qwen3Model
from transformers import AutoTokenizer

# --- Pinned model identity (docs/models.md / HANDOFF S4 model_ver rule) ---
# Changing either of these two lines is a MODEL VERSION CHANGE: bump
# MODEL_VER too, and the operator must run the version-switch procedure
# documented in README.md (set active_embed_model_ver -> restart kahyad ->
# `kahya reindex --re-embed`) - never edit them in place and expect
# existing chunk_vec rows to remain meaningful.
MODEL_REPO = "Qwen/Qwen3-Embedding-0.6B"
MODEL_REVISION = "97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3"
MODEL_VER = "qwen3-embedding-0.6b:512:v1"

MODEL_NAME = "qwen3-embedding-0.6b"  # the "model" field POST /v1/embeddings expects
EMBED_DIM = 512  # MRL truncation target (HANDOFF S4: "512-dim MRL")
MAX_BATCH = 64  # W12-11 step 1: "batch <= 64 inputs per request"
MAX_TOKENS = 2048  # generous ceiling for a memory-chunk-sized input; longer inputs are truncated, never errored


class BatchTooLargeError(ValueError):
    pass


class InvalidInputError(ValueError):
    pass


class Embedder:
    """Loads Qwen3-Embedding-0.6B once and serves embed() calls against it.

    A single mlx array-eval pipeline is not thread-safe to call
    concurrently from Python (mx.eval mutates shared Metal command-buffer
    state) - _lock serializes calls. ThreadingHTTPServer still lets
    concurrent /health checks through instantly (they never touch this
    lock), so a slow embed call never blocks liveness polling.
    """

    def __init__(self) -> None:
        # local_files_only=True (W12-11 task spec: "do NOT re-download;
        # use the pinned revision") - a missing/incomplete local snapshot
        # raises here rather than silently reaching out to the network,
        # which the supervisor (kahyad/internal/mlxsup) then reports as a
        # crash-and-backoff-restart, exactly like any other startup
        # failure.
        path = snapshot_download(MODEL_REPO, revision=MODEL_REVISION, local_files_only=True)

        with open(os.path.join(path, "config.json"), encoding="utf-8") as f:
            cfg = json.load(f)

        args = ModelArgs(
            model_type=cfg["model_type"],
            hidden_size=cfg["hidden_size"],
            num_hidden_layers=cfg["num_hidden_layers"],
            intermediate_size=cfg["intermediate_size"],
            num_attention_heads=cfg["num_attention_heads"],
            rms_norm_eps=cfg["rms_norm_eps"],
            vocab_size=cfg["vocab_size"],
            num_key_value_heads=cfg["num_key_value_heads"],
            max_position_embeddings=cfg["max_position_embeddings"],
            rope_theta=cfg["rope_theta"],
            head_dim=cfg["head_dim"],
            tie_word_embeddings=cfg["tie_word_embeddings"],
            rope_scaling=cfg.get("rope_scaling"),
        )

        model = Qwen3Model(args)
        weights = mx.load(os.path.join(path, "model.safetensors"))
        model.load_weights(list(weights.items()))
        mx.eval(model.parameters())

        self._model = model
        self._tokenizer = AutoTokenizer.from_pretrained(path)
        self._lock = threading.Lock()

    def embed_batch(self, texts: list[str]) -> tuple[list[list[float]], int]:
        """Returns (vectors, total_prompt_tokens) - one 512-float,
        L2-normalized vector per input text, in the SAME order as texts.
        """
        vectors: list[list[float]] = []
        total_tokens = 0
        with self._lock:
            for text in texts:
                vec, n_tokens = self._embed_one(text)
                vectors.append(vec)
                total_tokens += n_tokens
        return vectors, total_tokens

    def _embed_one(self, text: str) -> tuple[list[float], int]:
        # One sequence at a time, no padding: Qwen3Model's own
        # create_attention_mask (mlx_lm.models.base) builds a plain causal
        # mask with no notion of a padding mask, so batching sequences of
        # different lengths together via naive padding would let real
        # tokens' attention leak onto pad-token embeddings. A single
        # unpadded sequence sidesteps the whole problem - simple and
        # provably correct, at a modest throughput cost that does not
        # matter at kahyad's corpus scale (HANDOFF S4: "<=~100k chunks").
        ids = self._tokenizer(text, truncation=True, max_length=MAX_TOKENS)["input_ids"]
        if not ids:
            ids = [self._tokenizer.pad_token_id or 0]

        x = mx.array(ids)[None, :]
        hidden = self._model(x)  # (1, L, hidden_size), post-final-RMSNorm

        last = hidden[0, -1, :].astype(mx.float32)  # LAST-TOKEN pooling
        full_norm = mx.sqrt(mx.sum(last * last))
        last = last / full_norm  # L2-normalize the full-width vector first

        truncated = last[:EMBED_DIM]  # MRL truncation
        trunc_norm = mx.sqrt(mx.sum(truncated * truncated))
        truncated = truncated / trunc_norm  # re-normalize after truncation
        mx.eval(truncated)

        return truncated.tolist(), len(ids)


class Handler(http.server.BaseHTTPRequestHandler):
    embedder: Embedder | None = None  # set by main() before serve_forever

    def _write_json(self, status: int, obj: dict) -> None:
        body = json.dumps(obj).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802 (BaseHTTPRequestHandler's own naming convention)
        if self.path == "/health":
            self._write_json(200, {"status": "ok", "model_ver": MODEL_VER})
            return
        self._write_json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/v1/embeddings":
            self._write_json(404, {"error": "not found"})
            return

        length = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(length) if length else b""
        try:
            body = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            self._write_json(400, {"error": "invalid JSON body"})
            return

        try:
            inputs = self._validate_inputs(body)
        except BatchTooLargeError as e:
            self._write_json(400, {"error": str(e)})
            return
        except InvalidInputError as e:
            self._write_json(400, {"error": str(e)})
            return

        assert self.embedder is not None  # main() always sets this before serving
        vectors, total_tokens = self.embedder.embed_batch(inputs)

        data = [
            {"object": "embedding", "index": i, "embedding": vec}
            for i, vec in enumerate(vectors)
        ]
        self._write_json(200, {
            "object": "list",
            "data": data,
            "model": body.get("model", MODEL_NAME),
            "usage": {"prompt_tokens": total_tokens, "total_tokens": total_tokens},
        })

    @staticmethod
    def _validate_inputs(body: dict) -> list[str]:
        inputs = body.get("input")
        if not isinstance(inputs, list) or not inputs:
            raise InvalidInputError("input must be a non-empty array of strings")
        if not all(isinstance(s, str) for s in inputs):
            raise InvalidInputError("every input element must be a string")
        if len(inputs) > MAX_BATCH:
            raise BatchTooLargeError(f"batch of {len(inputs)} exceeds max {MAX_BATCH} inputs per request")
        return inputs

    def log_message(self, fmt: str, *args) -> None:  # noqa: A002
        # Diagnostics only, to stderr (kahyad/internal/mlxsup discards the
        # child's stdout/stderr rather than parsing it - this line is for
        # a human running the service by hand, e.g. via `make test-mlx`).
        sys.stderr.write("%s - - %s\n" % (self.address_string(), fmt % args))


def main() -> None:
    port = int(os.environ.get("KAHYA_EMBED_PORT", "8092"))
    embedder = Embedder()  # model load happens BEFORE binding/serving
    Handler.embedder = embedder

    # 127.0.0.1 ONLY (HANDOFF S4 supervision contract, verbatim: "127.0.0.1'e
    # bagli TCP HTTP sunucusu") - never 0.0.0.0, never a wildcard bind.
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
    httpd.serve_forever()


if __name__ == "__main__":
    main()
