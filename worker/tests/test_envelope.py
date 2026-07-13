"""Envelope v1 validation matrix (W12-09 task spec step 7), mirroring
kahyad/internal/spawn.Envelope.Validate's own test matrix one for one."""

import json
import unittest

import _pathfix  # noqa: F401

from kahya_worker.envelope import ALLOWED_MODELS, EnvelopeError, parse_envelope


def valid_envelope_dict() -> dict:
    return {
        "schema_version": 1,
        "task_id": "t_9f2c1a4b5e6d7f8091a2b3c4d5e6f708",
        "trace_id": "3f9a6b2c1d4e5f60718293a4b5c6d7e8",
        "session_id": None,
        "kind": "chat",
        "prompt": "Kadıköy'deki randevuyu hatırlat",
        "model": "claude-sonnet-5",
        "memory_injection": True,
        "created_at": "2026-07-10T12:00:00Z",
    }


def encode(d: dict) -> bytes:
    return json.dumps(d).encode("utf-8")


class TestParseEnvelope(unittest.TestCase):
    def test_accepts_well_formed_envelope(self) -> None:
        env = parse_envelope(encode(valid_envelope_dict()))
        self.assertEqual(env.task_id, "t_9f2c1a4b5e6d7f8091a2b3c4d5e6f708")
        self.assertEqual(env.prompt, "Kadıköy'deki randevuyu hatırlat")
        self.assertTrue(env.memory_injection)
        self.assertIsNone(env.session_id)

    def test_accepts_string_session_id(self) -> None:
        d = valid_envelope_dict()
        d["session_id"] = "sess-123"
        env = parse_envelope(encode(d))
        self.assertEqual(env.session_id, "sess-123")

    def test_rejects_invalid_json(self) -> None:
        with self.assertRaises(EnvelopeError):
            parse_envelope(b"not json{{{")

    def test_rejects_non_object_json(self) -> None:
        with self.assertRaises(EnvelopeError):
            parse_envelope(b"[1,2,3]")

    def test_rejects_missing_field(self) -> None:
        for field in (
            "schema_version", "task_id", "trace_id", "session_id",
            "kind", "prompt", "model", "memory_injection", "created_at",
        ):
            with self.subTest(field=field):
                d = valid_envelope_dict()
                del d[field]
                with self.assertRaises(EnvelopeError):
                    parse_envelope(encode(d))

    def test_rejects_unknown_schema_version(self) -> None:
        d = valid_envelope_dict()
        d["schema_version"] = 2
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_blank_task_id(self) -> None:
        d = valid_envelope_dict()
        d["task_id"] = "   "
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_blank_trace_id(self) -> None:
        d = valid_envelope_dict()
        d["trace_id"] = ""
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_non_string_session_id(self) -> None:
        d = valid_envelope_dict()
        d["session_id"] = 123
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_wrong_kind(self) -> None:
        d = valid_envelope_dict()
        d["kind"] = "batch"
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_blank_prompt(self) -> None:
        d = valid_envelope_dict()
        d["prompt"] = "   "
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_model_not_in_section_9_set(self) -> None:
        d = valid_envelope_dict()
        d["model"] = "gpt-5"
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_accepts_every_allowed_model(self) -> None:
        for model in ALLOWED_MODELS:
            with self.subTest(model=model):
                d = valid_envelope_dict()
                d["model"] = model
                parse_envelope(encode(d))  # must not raise

    def test_rejects_non_bool_memory_injection(self) -> None:
        d = valid_envelope_dict()
        d["memory_injection"] = "true"
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_blank_created_at(self) -> None:
        d = valid_envelope_dict()
        d["created_at"] = ""
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    # --- W4-02: resume ---

    def test_resume_defaults_to_false_when_absent(self) -> None:
        env = parse_envelope(encode(valid_envelope_dict()))
        self.assertFalse(env.resume)

    def test_accepts_resume_true_with_session_id(self) -> None:
        d = valid_envelope_dict()
        d["session_id"] = "sess-123"
        d["resume"] = True
        env = parse_envelope(encode(d))
        self.assertTrue(env.resume)
        self.assertEqual(env.session_id, "sess-123")

    def test_rejects_resume_true_without_session_id(self) -> None:
        d = valid_envelope_dict()
        d["resume"] = True  # session_id left None
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_resume_true_with_blank_session_id(self) -> None:
        d = valid_envelope_dict()
        d["session_id"] = "   "
        d["resume"] = True
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_non_bool_resume(self) -> None:
        d = valid_envelope_dict()
        d["resume"] = "true"
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    # --- W4-03: reader mode/schema ---

    def test_mode_defaults_to_empty_when_absent(self) -> None:
        env = parse_envelope(encode(valid_envelope_dict()))
        self.assertEqual(env.mode, "")
        self.assertIsNone(env.schema)

    def test_accepts_mode_reader_with_schema(self) -> None:
        d = valid_envelope_dict()
        d["mode"] = "reader"
        d["schema"] = "mail_summary_v1"
        env = parse_envelope(encode(d))
        self.assertEqual(env.mode, "reader")
        self.assertEqual(env.schema, "mail_summary_v1")

    def test_rejects_mode_reader_without_schema(self) -> None:
        d = valid_envelope_dict()
        d["mode"] = "reader"
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_mode_reader_with_blank_schema(self) -> None:
        d = valid_envelope_dict()
        d["mode"] = "reader"
        d["schema"] = "   "
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_unknown_mode(self) -> None:
        d = valid_envelope_dict()
        d["mode"] = "actor"
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_non_string_mode(self) -> None:
        d = valid_envelope_dict()
        d["mode"] = 1
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    # --- W6-02: stt mode/input_audio_path ---

    def test_input_audio_path_defaults_to_none(self) -> None:
        env = parse_envelope(encode(valid_envelope_dict()))
        self.assertIsNone(env.input_audio_path)

    def test_accepts_mode_stt_with_input_audio_path(self) -> None:
        d = valid_envelope_dict()
        d["mode"] = "stt"
        d["input_audio_path"] = "/Users/x/Library/Application Support/Kahya/tmp/ptt-1.wav"
        env = parse_envelope(encode(d))
        self.assertEqual(env.mode, "stt")
        self.assertEqual(
            env.input_audio_path, "/Users/x/Library/Application Support/Kahya/tmp/ptt-1.wav"
        )

    def test_rejects_mode_stt_without_input_audio_path(self) -> None:
        d = valid_envelope_dict()
        d["mode"] = "stt"
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_mode_stt_with_blank_input_audio_path(self) -> None:
        d = valid_envelope_dict()
        d["mode"] = "stt"
        d["input_audio_path"] = "   "
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_rejects_non_string_input_audio_path(self) -> None:
        d = valid_envelope_dict()
        d["input_audio_path"] = 7
        with self.assertRaises(EnvelopeError):
            parse_envelope(encode(d))

    def test_input_audio_path_ignored_by_ordinary_chat_mode(self) -> None:
        # Set but mode is still "" (ordinary chat) - accepted; only
        # mode="stt" ever reads this field (kahya_worker.__main__'s own
        # dispatch), so an ordinary chat envelope carrying it is not
        # itself an error.
        d = valid_envelope_dict()
        d["input_audio_path"] = "/tmp/x.wav"
        env = parse_envelope(encode(d))
        self.assertEqual(env.mode, "")
        self.assertEqual(env.input_audio_path, "/tmp/x.wav")


if __name__ == "__main__":
    unittest.main()
