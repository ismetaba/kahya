# Local model fleet pins (W0-03)

The v1 local fleet is exactly these three models (HANDOFF §4). Later tasks (W3-08,
W12-11, W6-02) download nothing — they load exactly these revisions from the default
Hugging Face cache (`~/.cache/huggingface/hub`).

| §4 model | HF repo id | Revision SHA | On-disk size | Downloaded |
|---|---|---|---|---|
| Qwen3-30B-A3B (MLX 4-bit) | `mlx-community/Qwen3-30B-A3B-4bit` | `d388dead1515f5e085ef7a0431dd8fadf0886c57` | 16.4 GB | 2026-07-10 |
| whisper-large-v3-turbo (MLX) | `mlx-community/whisper-large-v3-turbo` | `a4aaeec0636e6fef84abdcbe3544cb2bf7e9f6fb` | 1.5 GB | 2026-07-10 |
| Qwen3-Embedding-0.6B | `Qwen/Qwen3-Embedding-0.6B` | `97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3` | 1.2 GB | 2026-07-10 |

Note on `Qwen3-Embedding-0.6B`: W12-11 derives the active `model_ver` tag from this
repo id + revision; per §4 ⚑ `model_ver` rule, changing this row later requires a full
re-embed from the Markdown source of truth (mixed-version KNN is forbidden).

TTS: `say -v '?' | grep -i yelda` verified present on 2026-07-10
(`Yelda tr_TR # Merhaba, benim adım Yelda.`) — no user action needed.
