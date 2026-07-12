// Package cloudretry implements the W4-04 cloud-call error taxonomy
// (HANDOFF §6 W4 ⚑, verbatim): "bulut çağrı hata taksonomisi (retryable
// 429/5xx/ağ + üstel backoff + max deneme + bekliyor-yeniden-deneme
// durumu)". This file (taxonomy.go) is the classifier
// (Classify(status, err) Class); backoff.go is the jittered exponential
// backoff schedule. Neither file talks to a store, a proxy, or a task —
// both are pure, deterministic, and trivially unit-testable so every
// other package (kahyad/internal/anthproxy's inline retry loop,
// kahyad/internal/task's park/fail-immediately decision) can share the
// exact same classification without duplicating the status/error list.
package cloudretry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Class is a cloud-call failure's retryability verdict — always one of
// exactly two values (never a third "unknown": Classify must return a
// deterministic answer for every input so callers never have to
// special-case "couldn't decide").
type Class int

const (
	// Retryable failures are worth another inline attempt (task spec step
	// 2's backoff loop) before falling back to task-level parking.
	Retryable Class = iota
	// NonRetryable failures surface immediately — no inline retry, no
	// task-level retry-wait: the task fails now with a clean Turkish
	// error (task spec step 6).
	NonRetryable
)

// String implements fmt.Stringer — used only for logging/debugging.
func (c Class) String() string {
	if c == Retryable {
		return "retryable"
	}
	return "non_retryable"
}

// retryableStatuses is the task spec's CLOSED list (step 1, verbatim):
// "408, 429, 500, 502, 503, 504, 529 (Anthropic overloaded)". Every other
// HTTP status — including every other 4xx/5xx — is NonRetryable; the task
// spec's own "(and any other 4xx not listed above)" clause for the 4xx
// side generalizes cleanly to "anything not on this list" for the whole
// taxonomy, since Class has no third value.
var retryableStatuses = map[int]bool{
	408: true, // Request Timeout
	429: true, // Too Many Requests (rate limit)
	500: true, // Internal Server Error
	502: true, // Bad Gateway
	503: true, // Service Unavailable
	504: true, // Gateway Timeout
	529: true, // Anthropic-specific: overloaded
}

// Classify implements the task spec's error taxonomy (step 1). status is
// the upstream HTTP status actually received (0 means "no response was
// ever received — classify the transport error instead"); err is
// whatever the RoundTrip attempt returned (nil on a real HTTP response,
// even an error-status one).
//
// status takes precedence whenever a real response arrived: a transport
// succeeded in getting SOME answer from the upstream, so its status code
// alone decides retryability, regardless of any incidental non-nil err
// (e.g. a body-read error after a 200) — that class of error is not this
// function's concern (see the doc comment on the err-only branch below
// for why status==0 is what actually selects it).
func Classify(status int, err error) Class {
	if status != 0 {
		if retryableStatuses[status] {
			return Retryable
		}
		return NonRetryable
	}
	return classifyTransportErr(err)
}

// classifyTransportErr implements the task spec's transport-error half
// (step 1, verbatim): "DNS failure, connection refused/reset, TLS
// handshake timeout, context.DeadlineExceeded" are Retryable; anything
// else observed with no HTTP response at all is NonRetryable (a closed
// list, exactly mirroring the HTTP-status half's own closed list).
func classifyTransportErr(err error) Class {
	if err == nil {
		// Should not happen in practice (Classify is only ever called
		// after an attempt has already failed one way or the other), but
		// NonRetryable is the safe, non-looping default for "nothing to
		// classify".
		return NonRetryable
	}

	// context.DeadlineExceeded: the request's own deadline elapsed
	// (kahyad/internal/anthproxy's task_timeout_min context, or a dial/
	// handshake timeout surfaced this way by net/http).
	if errors.Is(err, context.DeadlineExceeded) {
		return Retryable
	}

	// DNS failure.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return Retryable
	}

	// Connection refused/reset — checked via errors.Is against the
	// syscall.Errno values net.OpError/os.SyscallError wrap, so this
	// matches regardless of how deep net/http buried the underlying
	// errno.
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return Retryable
	}

	// Any other transport-level failure net/http reports as a
	// *net.OpError (dial failure, TLS handshake timeout not already
	// caught above, write/read failure mid-connection) is still a
	// connectivity problem worth retrying — this is deliberately the
	// broadest of the four buckets (task spec: "TLS handshake timeout" is
	// exactly this shape: a *net.OpError wrapping a timeout).
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return Retryable
	}

	// Catch-all: any remaining error that identifies itself as a timeout
	// via the net.Error interface (e.g. a bare timeout not wrapped in
	// *net.OpError/*net.DNSError).
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return Retryable
	}

	return NonRetryable
}

// ParseRetryAfter parses an HTTP Retry-After header value (task spec step
// 1: "Honor a retry-after header when present (cap 60s)"), supporting
// both forms RFC 9110 §10.2.3 defines — a delta-seconds integer, or an
// HTTP-date — and capping the result at 60s either way. ok is false when
// header is empty or unparseable in either form (the caller falls back
// to exponential backoff, per the task spec).
func ParseRetryAfter(header string) (d time.Duration, ok bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(header); err == nil {
		if secs < 0 {
			secs = 0
		}
		return capRetryAfter(time.Duration(secs) * time.Second), true
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return capRetryAfter(d), true
	}
	return 0, false
}

// maxRetryAfter is the task spec's own cap (step 1: "cap 60s").
const maxRetryAfter = 60 * time.Second

func capRetryAfter(d time.Duration) time.Duration {
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// ReasonForStatus returns a short, stable, ENGLISH technical identifier
// for a NonRetryable HTTP status — CLAUDE.md's language policy is
// explicit that user-facing strings are Turkish but "technical output
// stays English"; this is exactly the "<sebep>" substituted into the
// task spec's non-retryable Turkish notification string (step 6: a
// short English API error id). The named cases mirror Anthropic's own
// documented error "type" values for the statuses that have one; the
// default covers any other 4xx the taxonomy still classifies
// NonRetryable.
func ReasonForStatus(status int) string {
	switch status {
	case http.StatusBadRequest: // 400
		return "invalid_request_error"
	case http.StatusUnauthorized: // 401
		return "authentication_error"
	case http.StatusForbidden: // 403
		return "permission_error"
	case http.StatusNotFound: // 404
		return "not_found_error"
	case http.StatusRequestEntityTooLarge: // 413
		return "request_too_large"
	case http.StatusUnprocessableEntity: // 422
		return "unprocessable_entity"
	default:
		return fmt.Sprintf("http_%d", status)
	}
}
