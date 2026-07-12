package anthproxy

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"kahya/kahyad/internal/cloudretry"
)

// defaultMaxInlineRetries mirrors config.Config's own cloud_retry_max_
// inline default (task spec: 3) - kept as a local literal, the same
// no-config-import convention this package already established for
// CredentialMode's string literals (see proxy.go's package doc comment).
const defaultMaxInlineRetries = 3

// ErrRetriesExhausted is the sentinel error retryTransport.RoundTrip
// returns once every inline attempt for one logical call has come back
// Retryable (task spec step 3: "when inline retries are exhausted, the
// proxy returns a typed error to the worker"). httputil.ReverseProxy
// routes a non-nil RoundTrip error to Proxy.handleUpstreamError (never
// ModifyResponse - see that method's own doc comment for why this is
// exactly the split this package needs between "exhausted" and every
// other outcome).
var ErrRetriesExhausted = errors.New("anthproxy: cloud upstream retries exhausted")

// ErrNonRetryable is the sentinel retryTransport.RoundTrip wraps around a
// TRANSPORT-level NonRetryable failure (resp==nil, err!=nil - e.g. a TLS
// certificate error or a scheme mismatch that cloudretry.Classify verdicts
// NonRetryable). An HTTP-STATUS NonRetryable outcome (a real 4xx response,
// resp!=nil, err==nil) is returned as (resp, nil) and reaches
// ModifyResponse (wrapResponseBody), which is where OnNonRetryableFailure
// fires for it. But a transport-level failure has no response at all, so
// RoundTrip must return a non-nil error - and httputil.ReverseProxy routes
// EVERY non-nil RoundTrip error to the ErrorHandler
// (Proxy.handleUpstreamError), NEVER ModifyResponse. Without a sentinel to
// distinguish it from a bug-shaped generic error, that path would fall
// through to a plain 502 and NEVER fire OnNonRetryableFailure - leaving the
// task stuck in 'executing' and (via the outbox redispatch loop)
// re-dispatched forever with no give-up. handleUpstreamError branches on
// this sentinel to fire OnNonRetryableFailure exactly as the HTTP-status
// path's ModifyResponse does.
var ErrNonRetryable = errors.New("anthproxy: cloud upstream non-retryable transport failure")

// retryAttemptLogger is called once per upstream attempt (task spec step
// 2: "each retry logs JSONL with trace_id, attempt number, status").
// errMsg is empty on a real HTTP response, even an error-status one.
type retryAttemptLogger func(attempt int, status int, errMsg string)

// retryTransport wraps a base http.RoundTripper with the W4-04 inline
// retry loop (task spec steps 1-2): Retryable outcomes
// (kahyad/internal/cloudretry.Classify) are retried up to maxInline
// attempts total with jittered exponential backoff (or the upstream's own
// Retry-After header, capped at 60s - cloudretry.ParseRetryAfter);
// success and NonRetryable outcomes return immediately after the FIRST
// attempt that produces one, never retried.
//
// Exactly one call is ever billed to the cost governor for the whole
// logical request: ModifyResponse (proxy.go's wrapResponseBody) only ever
// runs when RoundTrip returns (resp, nil) - true for success and for a
// NonRetryable single attempt, but NEVER for the all-retryable-exhausted
// path (which always returns (nil, err) - see the loop's final return) -
// so RecordUsage is called at most once per logical call, regardless of
// how many upstream attempts happened inside this one RoundTrip.
type retryTransport struct {
	base      http.RoundTripper
	maxInline int
	backoff   cloudretry.Backoff
	sleep     func(time.Duration)
	log       retryAttemptLogger
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	data, _ := req.Context().Value(reqDataCtxKey{}).(*reqData)

	maxInline := t.maxInline
	if maxInline <= 0 {
		maxInline = defaultMaxInlineRetries
	}

	var lastErr error
	for attempt := 1; attempt <= maxInline; attempt++ {
		attemptReq := req
		if attempt > 1 {
			attemptReq = req.Clone(req.Context())
			if req.GetBody != nil {
				body, gerr := req.GetBody()
				if gerr != nil {
					return nil, fmt.Errorf("anthproxy: replay request body for retry attempt %d: %w", attempt, gerr)
				}
				attemptReq.Body = body
			}
		}

		resp, err := t.base.RoundTrip(attemptReq)
		status := 0
		errMsg := ""
		if resp != nil {
			status = resp.StatusCode
		}
		if err != nil {
			errMsg = err.Error()
		}
		if t.log != nil {
			t.log(attempt, status, errMsg)
		}

		if err == nil && status < 400 {
			if data != nil {
				data.RetryAttempts = attempt
			}
			return resp, nil // success - never retried further
		}

		class := cloudretry.Classify(status, err)
		if data != nil {
			data.RetryAttempts = attempt
		}
		if class == cloudretry.NonRetryable {
			reason := cloudretry.ReasonForStatus(status)
			if status == 0 {
				// Transport-level NonRetryable: there is no HTTP status, so
				// ReasonForStatus(0) would yield the meaningless "http_0" -
				// use a stable, English transport identifier instead (this is
				// the <sebep> substituted into the Turkish failure string).
				reason = "transport_error"
			}
			if data != nil {
				data.NonRetryableReason = reason
			}
			if err != nil {
				// Transport-level NonRetryable (resp==nil): a non-nil RoundTrip
				// error is routed by httputil.ReverseProxy to the ErrorHandler
				// (handleUpstreamError), NOT ModifyResponse - so
				// wrapResponseBody's OnNonRetryableFailure hook can never fire
				// for this path. Wrap in ErrNonRetryable so handleUpstreamError
				// fires it there instead. (See ErrNonRetryable's doc comment.)
				if resp != nil {
					_ = resp.Body.Close()
				}
				return nil, fmt.Errorf("%w: %v", ErrNonRetryable, err)
			}
			return resp, err // HTTP-status NonRetryable: (resp, nil) -> ModifyResponse fires the hook
		}

		// Retryable: close this attempt's response body (never returned to
		// any caller) before either retrying or giving up.
		lastErr = err
		if resp != nil {
			_ = resp.Body.Close()
		}
		if attempt < maxInline {
			t.sleepBeforeRetry(attempt, resp)
		}
	}

	if data != nil {
		data.Exhausted = true
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrRetriesExhausted, lastErr)
	}
	return nil, ErrRetriesExhausted
}

// sleepBeforeRetry waits the delay before the NEXT attempt: the
// upstream's own Retry-After header when the just-finished attempt sent
// one (task spec step 1: "Honor a retry-after header when present"),
// otherwise the jittered exponential backoff schedule.
func (t *retryTransport) sleepBeforeRetry(attempt int, resp *http.Response) {
	var delay time.Duration
	if resp != nil {
		if d, ok := cloudretry.ParseRetryAfter(resp.Header.Get("Retry-After")); ok {
			delay = d
		}
	}
	if delay <= 0 {
		delay = t.backoff.Delay(attempt)
	}
	if t.sleep != nil {
		t.sleep(delay)
		return
	}
	time.Sleep(delay)
}
