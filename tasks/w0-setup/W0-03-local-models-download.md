# W0-03 — Local models download

**Status:** todo
**Phase:** W0 — Day-1 setup
**Depends on:** none
**Flags:** long-running
**Handoff refs:** §4 models, §7

## Goal

The exact three local models of the v1 fleet are fully downloaded into the Hugging Face cache
with recorded revisions, and the `Yelda` Turkish TTS voice is verified (or flagged to the user).
Nothing is served or loaded yet — W3-08 (secret lane), W12-11 (embeddings) and W6-02 (whisper)
consume these files.

## Context you need

HANDOFF §4 (binding, quote verbatim):

> Yerel filo v1'de **tam üç** yerleşik model: `whisper-large-v3-turbo`, `Qwen3-Embedding-0.6B` (512-dim MRL, `model_ver` etiketli), `Qwen3-30B-A3B`. Reranker/120B/wake-word ertelendi.

> `say -v Yelda` (MVP). Piper/XTTS ertelendi. ⚑ Yelda sesi kutudan gelmeyebilir — Gün 1 kurulumda `say -v '?'` ile doğrula, yoksa Sistem Ayarları'ndan indir

HANDOFF §7 (binding):

> # 3) Yerel modelleri indir (~20GB, resume destekli — ComfyUI ile bellek çekişmesine dikkat)

> say -v '?' | grep -i yelda   # ⚑ Türkçe ses mevcut mu? yoksa Sistem Ayarları > Erişilebilirlik'ten indir

Runtime context (do NOT implement here, but it fixes the formats): §4 IPC says
`mlx_lm.server` serves Qwen3-30B-A3B and `mlx-whisper` runs as a library — so the 30B and
whisper artifacts must be **MLX-format** repos; §4 routing says Qwen3-30B-A3B is
"MLX 4-bit yerel". §4: "⚑ **Bellek baskısı:** Qwen3-30B-A3B (~17GB) **talep-üzerine yüklenir,
boşta-TTL ile boşaltılır**" — download only, never load models in this task (ComfyUI/Wan may
be using the RAM).

Repo IDs (the HANDOFF gives placeholders `<Qwen3-30B-A3B-4bit>` etc.; these are the canonical
matches — verify each exists before downloading, and if an ID has drifted pick the official
MLX conversion of the SAME model, never a different model):

| §4 model | HF repo id |
|---|---|
| Qwen3-30B-A3B (MLX 4-bit) | `mlx-community/Qwen3-30B-A3B-4bit` |
| whisper-large-v3-turbo (MLX) | `mlx-community/whisper-large-v3-turbo` |
| Qwen3-Embedding-0.6B | `Qwen/Qwen3-Embedding-0.6B` |

## Deliverables

- All three models fully present in `~/.cache/huggingface/hub` (default cache; resume-capable).
- `/Users/matt/code/kahya/docs/models.md` — table: §4 model name, HF repo id, revision SHA,
  on-disk size, download date. This is the version pin for W3-08/W12-11/W6-02.
- Yelda voice verified, or a clean `blocked-user` flag with Turkish instructions.

## Steps

1. Disk preflight: `df -h ~` — require ≥ 25 GB free (models total ~20 GB). If not, stop and
   tell the user in Turkish how much space is needed. Also capture the MLX-process baseline
   (the user may legitimately be running ComfyUI/other MLX stacks — HANDOFF §3):
   `pgrep -f 'mlx_lm|mlx_whisper' | sort > /tmp/w0-03-mlx-before.txt; true`.
2. Install the HF CLI in an isolated venv (do not touch the worker venv; this task has no deps):
   ```bash
   python3 -m venv ~/.venvs/hf && ~/.venvs/hf/bin/pip install -U 'huggingface_hub[cli]'
   ```
3. Verify each repo id exists and capture its revision:
   ```bash
   ~/.venvs/hf/bin/python -c "from huggingface_hub import model_info; \
     print(model_info('mlx-community/Qwen3-30B-A3B-4bit').sha)"
   ```
   (repeat for the other two). If a repo 404s, pick the official replacement repo for the
   EXACT same model — for the two MLX-served artifacts (30B, whisper) it must be an MLX
   conversion; for the embedding model, the upstream `Qwen/` repo — and note the substitution
   in `docs/models.md`. Never substitute a different model: §4 locks the fleet to exactly
   these three.
4. Download sequentially (resume is automatic on re-run; run in background, verify after —
   this is the ⏳ part, ~20 GB):
   ```bash
   ~/.venvs/hf/bin/huggingface-cli download mlx-community/Qwen3-30B-A3B-4bit
   ~/.venvs/hf/bin/huggingface-cli download mlx-community/whisper-large-v3-turbo
   ~/.venvs/hf/bin/huggingface-cli download Qwen/Qwen3-Embedding-0.6B
   ```
   (If the CLI has been renamed `hf` in the installed version, `hf download <repo>` is the same.)
5. Verify completeness: `~/.venvs/hf/bin/huggingface-cli scan-cache` must list all three repos;
   sanity-check sizes (30B-4bit ≈ 16–18 GB, whisper-turbo ≈ 1.5–1.7 GB, embedding ≈ 1–1.5 GB);
   for each snapshot dir confirm `config.json` parses:
   `python3 -c "import json,glob; [json.load(open(p)) for p in glob.glob('<snapshot>/config.json')]"`.
6. Write `/Users/matt/code/kahya/docs/models.md` per Deliverables (repo id + revision SHA from
   step 3 — later tasks download nothing and load exactly these revisions). On the
   `Qwen3-Embedding-0.6B` row add the note: "W12-11 derives the active `model_ver` tag from
   this repo id + revision; per §4 ⚑ `model_ver` rule, changing this row later requires a full
   re-embed from the Markdown source of truth (mixed-version KNN is forbidden)."
7. TTS check: `say -v '?' | grep -i yelda`.
   - If found: optional smoke test `say -v Yelda 'Kâhya hazır.'`.
   - If missing: tell the user in Turkish — "Yelda sesi yüklü değil. Sistem Ayarları >
     Erişilebilirlik > Sözlü İçerik > Sistem sesi > Sesleri Yönet… yolundan Türkçe 'Yelda'
     sesini indirmen gerekiyor." — set `Status: blocked-user`, mark `[!]` in BACKLOG.md with
     that one-liner, and continue (the downloads themselves can still complete this task's
     other criteria).
8. Post-check: `pgrep -f 'mlx_lm|mlx_whisper' | sort | diff /tmp/w0-03-mlx-before.txt -`
   must show no additions — this task loads/serves nothing (§4 ⚑ memory pressure: the 30B is
   on-demand-loaded by kahyad in W3-08, never here; pre-existing user MLX processes are not
   this task's problem).
9. Commit `docs/models.md`: `[W0-03] record local model fleet pins (repo ids + revisions)`.

## Acceptance criteria

- [ ] `~/.venvs/hf/bin/huggingface-cli scan-cache | grep -c -E 'Qwen3-30B-A3B-4bit|whisper-large-v3-turbo|Qwen3-Embedding-0.6B'`
      prints 3.
- [ ] Sizes sane: `du -sm ~/.cache/huggingface/hub/models--<org>--<name>/snapshots/<sha>` per
      model prints 14000–20000 (30B-4bit), 1000–2500 (whisper-large-v3-turbo), 500–2500
      (Qwen3-Embedding-0.6B) MB — a value below range means a truncated download; re-run step 4.
- [ ] Each of the three snapshot dirs contains a parseable `config.json` (step 5 command exits 0).
- [ ] `/Users/matt/code/kahya/docs/models.md` exists, committed, and contains one row per model
      with a 40-char revision SHA.
- [ ] `say -v '?' | grep -i yelda` prints a line — OR this task is `blocked-user`/`[!]` in
      BACKLOG.md with the Turkish voice-install instruction.
- [ ] `pgrep -f 'mlx_lm|mlx_whisper' | sort | diff /tmp/w0-03-mlx-before.txt -` shows no lines
      added versus the step-1 baseline (no model was loaded/served by this task; normally both
      sides are empty).

## Out of scope

- Serving/supervision: `mlx_lm.server` spawn, port pinning, health-check, on-demand load,
  idle-TTL unload, free-memory check / fail-closed (W3-08).
- Embedding service, 512-dim MRL truncation, `model_ver` tagging, KNN (W12-11).
- Push-to-talk / `mlx-whisper` integration with `language=tr` (W6-02).
- Anything §8 defers: reranker, GPT-OSS-120B, wake-word, XTTS/Piper, intent-LoRA.
- Installing Python deps into `worker/.venv` (W0-02 owns the worker env).
