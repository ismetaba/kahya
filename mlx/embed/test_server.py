"""mlx/embed/test_server.py -- pytest for the embedding service (W12-11
step 6). Skips entirely unless the pinned Qwen3-Embedding-0.6B snapshot is
already present in the local HF cache (never downloads anything itself -
this is `make test-mlx` territory, not `make test`'s).

Exercises the REAL Embedder class directly (import, not a subprocess) so
these assertions run under plain `pytest` even in an environment where
this file's own process cannot reach a separate embedding-server process
over 127.0.0.1 loopback (same-process/same-thread use of the module is
unaffected either way) - the HTTP layer itself (request parsing, batch-size
validation, OpenAI-shaped response) is covered by
kahyad/internal/embed/client_test.go's httptest-based Go tests and by the
end-to-end Go cross-lingual test (build tag `mlx`), both of which stay
entirely in-process too.
"""

from __future__ import annotations

import os

import pytest
from huggingface_hub import try_to_load_from_cache

import server as embed_server

MODEL_PRESENT = (
    try_to_load_from_cache(
        embed_server.MODEL_REPO,
        "config.json",
        revision=embed_server.MODEL_REVISION,
    )
    is not None
)

pytestmark = pytest.mark.skipif(
    not MODEL_PRESENT,
    reason=f"{embed_server.MODEL_REPO}@{embed_server.MODEL_REVISION} not present in the local HF cache",
)


@pytest.fixture(scope="module")
def embedder() -> embed_server.Embedder:
    return embed_server.Embedder()


def _norm(vec: list[float]) -> float:
    return sum(x * x for x in vec) ** 0.5


def test_embed_produces_512_dims(embedder: embed_server.Embedder) -> None:
    vectors, _ = embedder.embed_batch(["hello world"])
    assert len(vectors) == 1
    assert len(vectors[0]) == embed_server.EMBED_DIM == 512


def test_embed_is_l2_normalized(embedder: embed_server.Embedder) -> None:
    vectors, _ = embedder.embed_batch(["The quick brown fox jumps over the lazy dog."])
    assert _norm(vectors[0]) == pytest.approx(1.0, abs=1e-3)


def test_embed_is_deterministic_on_repeat(embedder: embed_server.Embedder) -> None:
    text = "gold-token servisinde NATS saga deseni ve trace_id correlation notlari."
    v1, _ = embedder.embed_batch([text])
    v2, _ = embedder.embed_batch([text])
    assert v1 == v2


def test_turkish_input_embeds_without_error(embedder: embed_server.Embedder) -> None:
    vectors, n_tokens = embedder.embed_batch(["Kadıköy'de ev fiyatları"])
    assert len(vectors[0]) == 512
    assert _norm(vectors[0]) == pytest.approx(1.0, abs=1e-3)
    assert n_tokens > 0


def test_batch_preserves_order_and_dims(embedder: embed_server.Embedder) -> None:
    texts = ["birinci metin", "ikinci metin", "ucuncu metin"]
    vectors, _ = embedder.embed_batch(texts)
    assert len(vectors) == 3
    for v in vectors:
        assert len(v) == 512
    # Distinct inputs must not collapse to identical vectors.
    assert vectors[0] != vectors[1]
    assert vectors[1] != vectors[2]


def test_health_reports_pinned_model_ver() -> None:
    # A pure metadata check - no model load required, but kept in this
    # file (rather than always-run) since it exists purely to accompany
    # the model-dependent tests above; docs/models.md pins the exact
    # values this constant must equal.
    assert embed_server.MODEL_VER == "qwen3-embedding-0.6b:512:v1"
    assert embed_server.MODEL_REPO == "Qwen/Qwen3-Embedding-0.6B"
    assert embed_server.MODEL_REVISION == "97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3"


def test_validate_inputs_rejects_over_max_batch() -> None:
    with pytest.raises(embed_server.BatchTooLargeError):
        embed_server.Handler._validate_inputs({"input": ["x"] * (embed_server.MAX_BATCH + 1)})


def test_validate_inputs_rejects_empty_or_non_string() -> None:
    with pytest.raises(embed_server.InvalidInputError):
        embed_server.Handler._validate_inputs({"input": []})
    with pytest.raises(embed_server.InvalidInputError):
        embed_server.Handler._validate_inputs({"input": ["ok", 5]})
