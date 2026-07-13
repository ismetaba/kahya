// surface_cli.go implements the W3-06 local CLI approval surface's
// reusable I/O logic: `kahya approvals`'s pending-list formatting and
// `kahya approve <id>`'s W1/W2 ([e]vet/[h]ayır) vs. W3 (literal "onayla")
// decision prompts. The actual Turkish PROMPT TEXT constants live in
// kahyad/cmd/kahya/strings.go (CLAUDE.md convention: every CLI
// user-facing string collected in one file) — this package only supplies
// the generic read/compare mechanics both prompts share, parameterized by
// that text, so the byte-exact wording lives in exactly one place.
package approval

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"
)

// Decision is a normalized outcome from a CLI approval prompt.
type Decision int

const (
	DecisionDeny Decision = iota
	DecisionApprove
)

// PromptYesNo writes prompt to w (no added newline - callers control
// exact formatting), reads one line from r, and reports DecisionApprove
// iff the trimmed response case-insensitively equals one of yesWords —
// the W1/W2 "[e]vet/[h]ayır" gate. Any read error with no bytes at all
// read is returned as-is (DecisionDeny, err) — fail-closed: a broken
// input stream must never be treated as an approval.
func PromptYesNo(r *bufio.Reader, w io.Writer, prompt string, yesWords ...string) (Decision, error) {
	fmt.Fprint(w, prompt)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return DecisionDeny, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	for _, y := range yesWords {
		if line == strings.ToLower(y) {
			return DecisionApprove, nil
		}
	}
	return DecisionDeny, nil
}

// ReadTrimmedLine writes prompt to w, reads one line from r, and returns
// it with only the trailing line-framing bytes a terminal/pipe always
// adds stripped ("\n", then one "\r") — never any OTHER whitespace. This
// is PromptLiteral's own reading mechanics, factored out so a caller that
// ALSO needs the raw typed text itself (not just an approve/deny
// Decision) can get it — W6-01's `kahya approve <id>` W3 gate forwards
// this exact text to kahyad/internal/policy.Engine.Approve's own
// server-side byte-exact "onayla" check (the authoritative verification;
// this package's own comparison, via PromptLiteral below, is CLI-side UX
// only). Fail-closed identically to PromptYesNo/PromptLiteral on a broken
// input stream: an error with no bytes at all read returns ("", err).
func ReadTrimmedLine(r *bufio.Reader, w io.Writer, prompt string) (string, error) {
	fmt.Fprint(w, prompt)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, nil
}

// PromptLiteral reports DecisionApprove iff the response ReadTrimmedLine
// collects is EXACTLY literal, byte-for-byte and CASE-SENSITIVE (HANDOFF
// §5 safety #5 / this task's spec: W3 accepts nothing but the literally
// typed word "onayla" — not "evet", not "y", not "Onayla", and not
// " onayla"/"onayla " with stray whitespace either). Fail-closed
// identically to PromptYesNo on a broken input stream.
func PromptLiteral(r *bufio.Reader, w io.Writer, prompt, literal string) (Decision, error) {
	line, err := ReadTrimmedLine(r, w, prompt)
	if err != nil {
		return DecisionDeny, err
	}
	if line == literal {
		return DecisionApprove, nil
	}
	return DecisionDeny, nil
}

// PendingApprovalSummary is one `kahya approvals` row: id, tool, class,
// summary, age (this task's spec, verbatim field list).
type PendingApprovalSummary struct {
	ID      string
	Tool    string
	Class   string
	Summary string
	Age     time.Duration
}

// FormatApprovalsList renders items as one line per pending approval —
// full id (this task's spec: `kahya approve <id>` needs it typed/pasted
// whole, so the list must never truncate it), tool, class, summary, age —
// newest-request-first order left to the caller (kahyad's own listing
// query); this function only formats, never sorts or filters.
func FormatApprovalsList(items []PendingApprovalSummary) string {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "%-64s %-16s %-4s %-40s %s\n", it.ID, it.Tool, it.Class, it.Summary, formatAge(it.Age))
	}
	return b.String()
}

// formatAge renders d rounded to the second (e.g. "2m13s") — Go's own
// time.Duration.String() format, which is not Turkish text but a
// technical/log-style value (CLAUDE.md: technical output stays as-is;
// only the surrounding prose is Turkish).
func formatAge(d time.Duration) string {
	return d.Round(time.Second).String()
}
