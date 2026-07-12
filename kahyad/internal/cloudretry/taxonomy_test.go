package cloudretry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestClassifyStatusCodes is the step-8 table-driven taxonomy test,
// covering every status the task spec names.
func TestClassifyStatusCodes(t *testing.T) {
	cases := []struct {
		status int
		want   Class
	}{
		{408, Retryable},
		{429, Retryable},
		{500, Retryable},
		{502, Retryable},
		{503, Retryable},
		{504, Retryable},
		{529, Retryable},

		{400, NonRetryable},
		{401, NonRetryable},
		{403, NonRetryable},
		{404, NonRetryable},
		{413, NonRetryable},
		{422, NonRetryable},

		// Any other 4xx not explicitly listed - the task spec's own
		// clause.
		{418, NonRetryable},
		// Any other 5xx not on the retryable list.
		{501, NonRetryable},
		{505, NonRetryable},
		// A plain success status should never be asked about in
		// practice, but Classify must still return SOME deterministic
		// answer rather than panic.
		{200, NonRetryable},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("status_%d", c.status), func(t *testing.T) {
			if got := Classify(c.status, nil); got != c.want {
				t.Errorf("Classify(%d, nil) = %v, want %v", c.status, got, c.want)
			}
		})
	}
}

// TestClassifyTransportErrors covers the task spec's transport-error
// half: DNS failure, connection refused/reset, TLS handshake timeout
// (modeled as a *net.OpError wrapping a timeout), and
// context.DeadlineExceeded - all Retryable - plus a wrapped *net.OpError
// (proving errors.As unwraps through fmt.Errorf("%w", ...) wrapping, per
// the step-8 "wrapped net.OpError" requirement) and a generic
// unclassifiable error (NonRetryable, the safe default for the closed
// list).
func TestClassifyTransportErrors(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "api.anthropic.com", IsNotFound: true}
	connRefused := &net.OpError{Op: "dial", Net: "tcp", Err: errConnRefused{}}
	tlsTimeout := &net.OpError{Op: "tls handshake", Net: "tcp", Err: errTimeout{}}
	wrappedOpErr := fmt.Errorf("dial upstream: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("boom")})

	cases := []struct {
		name string
		err  error
		want Class
	}{
		{"dns_failure", dnsErr, Retryable},
		{"conn_refused_operr", connRefused, Retryable},
		{"tls_handshake_timeout", tlsTimeout, Retryable},
		{"context_deadline_exceeded", context.DeadlineExceeded, Retryable},
		{"context_deadline_exceeded_wrapped", fmt.Errorf("do request: %w", context.DeadlineExceeded), Retryable},
		{"wrapped_net_operror", wrappedOpErr, Retryable},
		{"generic_unclassified_error", errors.New("something else broke"), NonRetryable},
		{"nil_error", nil, NonRetryable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(0, c.err); got != c.want {
				t.Errorf("Classify(0, %v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestClassifyStatusTakesPrecedenceOverError proves a non-zero status
// always wins even when err happens to also be non-nil (e.g. a body-read
// glitch after a real error-status response arrived) - the task's own
// proxy code never hits this in practice (status/err are mutually
// exclusive from a single http.RoundTrip call), but Classify's contract
// must still be unambiguous.
func TestClassifyStatusTakesPrecedenceOverError(t *testing.T) {
	if got := Classify(503, errors.New("unrelated")); got != Retryable {
		t.Errorf("Classify(503, err) = %v, want Retryable", got)
	}
	if got := Classify(401, context.DeadlineExceeded); got != NonRetryable {
		t.Errorf("Classify(401, DeadlineExceeded) = %v, want NonRetryable", got)
	}
}

func TestClassString(t *testing.T) {
	if Retryable.String() != "retryable" {
		t.Errorf("Retryable.String() = %q, want retryable", Retryable.String())
	}
	if NonRetryable.String() != "non_retryable" {
		t.Errorf("NonRetryable.String() = %q, want non_retryable", NonRetryable.String())
	}
}

func TestReasonForStatus(t *testing.T) {
	cases := map[int]string{
		400: "invalid_request_error",
		401: "authentication_error",
		403: "permission_error",
		404: "not_found_error",
		413: "request_too_large",
		422: "unprocessable_entity",
		418: "http_418",
	}
	for status, want := range cases {
		if got := ReasonForStatus(status); got != want {
			t.Errorf("ReasonForStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestParseRetryAfterSeconds(t *testing.T) {
	d, ok := ParseRetryAfter("2")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if d != 2*time.Second {
		t.Errorf("d = %v, want 2s", d)
	}
}

func TestParseRetryAfterCapsAt60s(t *testing.T) {
	d, ok := ParseRetryAfter("120")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if d != 60*time.Second {
		t.Errorf("d = %v, want capped 60s", d)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	future := time.Now().UTC().Add(10 * time.Second).Truncate(time.Second)
	d, ok := ParseRetryAfter(future.Format(httpTimeFormat))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if d <= 0 || d > 11*time.Second {
		t.Errorf("d = %v, want ~10s", d)
	}
}

func TestParseRetryAfterEmptyOrInvalid(t *testing.T) {
	if _, ok := ParseRetryAfter(""); ok {
		t.Error("empty header: ok = true, want false")
	}
	if _, ok := ParseRetryAfter("not-a-number-or-date"); ok {
		t.Error("garbage header: ok = true, want false")
	}
}

// errConnRefused/errTimeout are minimal stand-ins used only to build
// *net.OpError values shaped like real connection-refused/TLS-handshake-
// timeout failures, without needing a real listener/dialer in this test.
type errConnRefused struct{}

func (errConnRefused) Error() string { return "connect: connection refused" }

type errTimeout struct{}

func (errTimeout) Error() string   { return "tls handshake timeout" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }

// httpTimeFormat mirrors net/http's TimeFormat constant
// ("Mon, 02 Jan 2006 15:04:05 GMT") without importing net/http just for a
// format string in this test file.
const httpTimeFormat = "Mon, 02 Jan 2006 15:04:05 GMT"
