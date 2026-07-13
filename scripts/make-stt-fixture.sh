#!/usr/bin/env bash
# make-stt-fixture.sh — generates worker/tests/fixtures/tr_toplanti.wav
# (W6-02 task spec step 7): a short Turkish utterance synthesized locally
# via `say -v Yelda` (HANDOFF §4 ⚑ Yelda note / W0-03's own voice check),
# converted to the mono 16kHz PCM16 wav mlx-whisper/ffmpeg's own capture
# path both expect. Run ONCE, then commit the resulting wav — this script
# itself is not part of `make test` (it needs the Yelda voice + `say`, a
# macOS-only, TCC-free system voice; the *fixture* is what tests use).
#
# If `say -v '?' | grep -i yelda` is empty, install the voice per HANDOFF
# §4 ⚑ (Sistem Ayarları > Erişilebilirlik > Konuşulan İçerik > Sesler) or
# ask the user to run this script themselves and commit the result.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture_dir="$repo_root/worker/tests/fixtures"
mkdir -p "$fixture_dir"

if ! say -v '?' | grep -qi yelda; then
  echo "make-stt-fixture.sh: the 'Yelda' voice is not installed - Sistem Ayarları > Erişilebilirlik > Konuşulan İçerik > Sesler'den ekleyin, sonra bu betiği tekrar çalıştırın." >&2
  exit 1
fi

tmp_aiff="$(mktemp -t tr_toplanti).aiff"
trap 'rm -f "$tmp_aiff"' EXIT

say -v Yelda -o "$tmp_aiff" "Yarın sabah dokuzda gold-token toplantım var."
afconvert -f WAVE -d LEI16@16000 -c 1 "$tmp_aiff" "$fixture_dir/tr_toplanti.wav"

echo "wrote $fixture_dir/tr_toplanti.wav"
