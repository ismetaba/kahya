// Package approval implements the W3-06 WYSIWYE approval payload: a
// deterministic, per-kind serializer (payload.go) whose SHA-256 the policy
// engine binds a one-time approval token to, and a byte-exact diff
// renderer (diff.go) that shows a human EXACTLY those bytes before they
// ever execute (HANDOFF §5 safety #5). The SAME Build* function runs at
// approval-mint time and at execution-verify time — that reuse (not two
// independently-maintained serializers) is the whole WYSIWYE invariant:
// kahyad/internal/policy's approvedBytesHash additionally routes every
// hash through kahyad/internal/canon.CanonicalizeBytes, so this package
// does not need to re-derive NFC/bidi/homoglyph handling itself for the
// low-level token-binding hash — it focuses on RENDERING (this package's
// own Hash field is a convenience for logging/display, not the value the
// policy engine's token is bound to).
package approval

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"

	"kahya/kahyad/internal/canon"
)

// Kind is one of the five WYSIWYE payload shapes this task's spec names.
type Kind string

const (
	KindFileEdit    Kind = "file_edit"
	KindShellScript Kind = "shell_script"
	KindOsascript   Kind = "osascript"
	KindEgress      Kind = "egress"
	KindMessage     Kind = "message"
)

// ApprovalPayload is the WYSIWYE approval unit: Summary+CanonicalBytes+
// Hash are shared with every Kind; the remaining fields are Kind-specific
// rendering material diff.go's renderer reads (never all populated at
// once — see each Build* function's own doc comment for which apply).
type ApprovalPayload struct {
	Kind Kind
	// Summary is a short, single-line, Turkish, human-readable
	// description (CLAUDE.md language policy: user-facing strings are
	// Turkish) - what `kahya approvals` lists.
	Summary string
	// CanonicalBytes is the deterministic serialized form this payload's
	// Hash is computed over - length-prefixed per field (never a plain
	// delimiter join) so no field's own content can be crafted to make
	// two semantically different payloads serialize identically.
	CanonicalBytes []byte
	// Hash is sha256(hex) of CanonicalBytes - a convenience for this
	// package's own callers (logging, `kahya approvals` listing);
	// kahyad/internal/policy's approval_tokens/pending_approvals hash
	// binding is computed independently, over canon-canonicalized
	// tool-call bytes (see this file's package doc comment).
	Hash string

	// ---- file_edit rendering material ----
	Path       string // canonicalized display path
	OldContent []byte // pre-image (nil if the file did not exist before)
	NewContent []byte // the bytes about to be written

	// ---- shell_script / osascript rendering material ----
	ImageDigest string // shell_script only ("" for osascript)
	Workdir     string // shell_script only (canonicalized display path)
	Script      []byte // shell_script and osascript

	// ---- egress rendering material ----
	Method    string
	Host      string // canonicalized display host
	ByteCount int64

	// ---- message rendering material ----
	Recipient string // canonicalized display recipient
	Body      string

	// Flags aggregates every canon.Flag surfaced while building this
	// payload's rendering material (path/host/recipient/body canon.Result
	// flags, plus any found scanning script/content text) - diff.go
	// renders each one as a visible escape or a mixed-script/confusable
	// warning line, per HANDOFF §5 safety #5: "never dropped invisibly".
	Flags []canon.Flag
}

// encodeFields serializes fields as [len(field) uint64 big-endian][field
// bytes]..., in the given fixed order - deterministic, and immune to a
// delimiter-collision ambiguity a plain "\n"-joined encoding would have
// (a field's own content can never be crafted to make two different
// (kind, fields) tuples hash identically).
func encodeFields(fields ...[]byte) []byte {
	var buf bytes.Buffer
	var lenBuf [8]byte
	for _, f := range fields {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(f)))
		buf.Write(lenBuf[:])
		buf.Write(f)
	}
	return buf.Bytes()
}

// hashOf returns sha256(hex) of b.
func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonText applies kahyad/internal/canon.Normalize to s for DISPLAY/path-
// identity purposes (the Canonical form - control code points stripped,
// confusables left untouched), returning both the canonical string and
// any flags found, so every Build* function below shares one path for
// "canonicalize this text field and surface its flags".
func canonText(s string) (string, []canon.Flag) {
	res := canon.Normalize(s)
	return res.Canonical, res.Flags
}

// BuildFileEdit builds a file_edit ApprovalPayload: canonical path + a
// unified diff of oldContent -> newContent. Content bytes are hashed and
// diffed EXACTLY as given (never NFC-transformed) — WYSIWYE requires the
// executed bytes to match the approved bytes bit-for-bit, and rewriting
// file CONTENT (as opposed to the identifying PATH) would defeat that.
// oldContent is nil when the target does not exist yet (a pure create).
func BuildFileEdit(rawPath string, oldContent, newContent []byte) ApprovalPayload {
	path, pathFlags := canonText(rawPath)
	diffText := UnifiedDiff(path, oldContent, newContent)

	canonicalBytes := encodeFields([]byte(KindFileEdit), []byte(path), []byte(diffText))

	p := ApprovalPayload{
		Kind: KindFileEdit, Path: path, OldContent: oldContent, NewContent: newContent,
		CanonicalBytes: canonicalBytes, Hash: hashOf(canonicalBytes),
		Summary: "fs_write: " + path,
		Flags:   append(append([]canon.Flag{}, pathFlags...), scanContentFlags(oldContent, newContent)...),
	}
	return p
}

// BuildShellScript builds a shell_script ApprovalPayload: image digest +
// canonical workdir + script bytes. Script bytes are hashed verbatim (the
// exact bytes fed to the sandboxed shell's stdin) - never NFC-transformed.
func BuildShellScript(imageDigest, rawWorkdir string, script []byte) ApprovalPayload {
	workdir, workdirFlags := canonText(rawWorkdir)
	canonicalBytes := encodeFields([]byte(KindShellScript), []byte(imageDigest), []byte(workdir), script)

	return ApprovalPayload{
		Kind: KindShellScript, ImageDigest: imageDigest, Workdir: workdir, Script: script,
		CanonicalBytes: canonicalBytes, Hash: hashOf(canonicalBytes),
		Summary: "shell_docker: " + workdir,
		Flags:   append(append([]canon.Flag{}, workdirFlags...), scanTextFlags(string(script))...),
	}
}

// BuildOsascript builds an osascript ApprovalPayload: script bytes only
// (HANDOFF §5 safety #6: osascript/JXA/Shortcuts bodies are the same
// "arbitrary code" class as shell, approved via this same WYSIWYE diff).
func BuildOsascript(script []byte) ApprovalPayload {
	canonicalBytes := encodeFields([]byte(KindOsascript), script)
	return ApprovalPayload{
		Kind: KindOsascript, Script: script,
		CanonicalBytes: canonicalBytes, Hash: hashOf(canonicalBytes),
		Summary: "osascript",
		Flags:   scanTextFlags(string(script)),
	}
}

// BuildEgress builds an egress ApprovalPayload: method + canonical host +
// byte count (HANDOFF §5 safety #1: "onay kartları egress sayılır ve aynı
// kapıdan geçer" - an egress-carrying approval card is itself subject to
// this same canonicalization/hash binding).
func BuildEgress(method, rawHost string, byteCount int64) ApprovalPayload {
	host, hostFlags := canonText(rawHost)
	countStr := strconv.FormatInt(byteCount, 10)
	canonicalBytes := encodeFields([]byte(KindEgress), []byte(method), []byte(host), []byte(countStr))

	return ApprovalPayload{
		Kind: KindEgress, Method: method, Host: host, ByteCount: byteCount,
		CanonicalBytes: canonicalBytes, Hash: hashOf(canonicalBytes),
		Summary: method + " " + host + " (" + countStr + " bayt)",
		Flags:   hostFlags,
	}
}

// BuildMessage builds a message ApprovalPayload: recipient + body (a W3
// "senin adına mesaj" action - mail_send, telegram_send, ...).
func BuildMessage(rawRecipient, body string) ApprovalPayload {
	recipient, recipientFlags := canonText(rawRecipient)
	canonicalBytes := encodeFields([]byte(KindMessage), []byte(recipient), []byte(body))

	return ApprovalPayload{
		Kind: KindMessage, Recipient: recipient, Body: body,
		CanonicalBytes: canonicalBytes, Hash: hashOf(canonicalBytes),
		Summary: "mesaj -> " + recipient,
		Flags:   append(append([]canon.Flag{}, recipientFlags...), scanTextFlags(body)...),
	}
}

// scanTextFlags returns canon.Normalize(s).Flags - a small helper so
// script/body scanning (surfaced for the rendered diff's warnings, never
// used to alter the hashed bytes) reads as one call at every Build* site.
func scanTextFlags(s string) []canon.Flag {
	return canon.Normalize(s).Flags
}

// scanContentFlags scans both old and new file_edit content for
// bidi/zero-width/mixed-script/confusable flags (e.g. a "Trojan Source"
// style bidi override hidden inside a source file's new content) -
// aggregated, not deduplicated, so diff.go can still report which side
// (old vs new) each flag came from if it chooses to.
func scanContentFlags(oldContent, newContent []byte) []canon.Flag {
	var flags []canon.Flag
	if len(oldContent) > 0 {
		flags = append(flags, scanTextFlags(string(oldContent))...)
	}
	if len(newContent) > 0 {
		flags = append(flags, scanTextFlags(string(newContent))...)
	}
	return flags
}
