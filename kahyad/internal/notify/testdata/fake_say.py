#!/usr/bin/env python3
"""fake_say.py - kahyad/internal/notify/tts_test.go (+ kahyad/internal/
halt/speech_test.go, kahyad/internal/ui/hscli_test.go)'s hermetic stand-in
for /usr/bin/say (W6-05): NEVER produces real audio and NEVER depends on a
real Yelda voice being installed anywhere - every `make test` run exercises
the REAL kahyad/internal/notify.Speaker code path (real exec.Command, real
process GROUP via Setpgid, real stdin pipe) against this deterministic
script instead of the real macOS `say` binary.

Made directly executable (chmod +x, shebang above) so a test can point
SpeakerConfig.SayBin straight at this file's own absolute path - exactly
the single-binary-path shape cfg.tts.say_bin has in production
(kahyad/internal/config.Config.TTSSayBin, default "/usr/bin/say").

Every invocation appends one "ARGV <space-joined argv>" line to
$FAKE_SAY_ARGV_LOG (if set).

"-v '?'" (voice-list query - kahyad/internal/notify.Speaker.voiceAvailable's
own call shape) mirrors the REAL `say -v '?'`: it never touches stdin, and
answers with $FAKE_SAY_VOICES verbatim (tests set/omit "Yelda" in this
string to flip the voice-installed/voice-missing branch).

Every OTHER invocation (an actual utterance) reads stdin to EOF, appends it
(plus a NUL separator, so multiple invocations across one test are
individually recoverable by splitting the log on "\\0") to
$FAKE_SAY_STDIN_LOG (if set), writes its OWN pid to $FAKE_SAY_READY_FILE (if
set - lets a test block until THIS exact invocation has actually started
sleeping, rather than racing it), appends "START <ns>"/"END <ns>"
wall-clock timestamps around an optional $FAKE_SAY_SLEEP_MS-millisecond
sleep to $FAKE_SAY_ARGV_LOG (the serialization test parses these to prove
no two invocations ever overlap), then exits $FAKE_SAY_EXIT_CODE (0 if
unset).
"""
import os
import sys
import time


def _append(path, line):
    if not path:
        return
    with open(path, "a", encoding="utf-8") as f:
        f.write(line + "\n")


def main():
    argv = sys.argv[1:]
    argv_log = os.environ.get("FAKE_SAY_ARGV_LOG")
    _append(argv_log, "ARGV " + " ".join(argv))

    if len(argv) >= 2 and argv[0] == "-v" and argv[1] == "?":
        # Real `say -v '?'` never reads stdin either - mirror that exactly.
        sys.stdout.write(os.environ.get("FAKE_SAY_VOICES", ""))
        sys.stdout.flush()
        return

    stdin_text = sys.stdin.buffer.read().decode("utf-8")
    stdin_log = os.environ.get("FAKE_SAY_STDIN_LOG")
    if stdin_log:
        with open(stdin_log, "a", encoding="utf-8") as f:
            f.write(stdin_text + "\x00")

    ready_file = os.environ.get("FAKE_SAY_READY_FILE")
    if ready_file:
        with open(ready_file, "w", encoding="utf-8") as f:
            f.write(str(os.getpid()))

    _append(argv_log, "START %d" % time.time_ns())
    sleep_ms = os.environ.get("FAKE_SAY_SLEEP_MS")
    if sleep_ms:
        time.sleep(float(sleep_ms) / 1000.0)
    _append(argv_log, "END %d" % time.time_ns())

    # `or "0"` (not a plain dict-get default) so an env var that is SET but
    # EMPTY (t.Setenv(..., "") - Go tests use this to reset a var between
    # subtests without fully unsetting it) is treated identically to unset,
    # rather than failing int("").
    sys.exit(int(os.environ.get("FAKE_SAY_EXIT_CODE") or "0"))


if __name__ == "__main__":
    main()
