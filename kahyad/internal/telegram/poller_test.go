package telegram

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	tele "gopkg.in/telebot.v4"
)

// newOfflineTeleBot builds a REAL *tele.Bot pointed at a fake Bot API
// server, offline (no getMe), with poller as its Poller.
func newOfflineTeleBot(t *testing.T, url string, poller tele.Poller) *tele.Bot {
	t.Helper()
	tb, err := tele.NewBot(tele.Settings{Offline: true, Token: "T", URL: url, Poller: poller})
	if err != nil {
		t.Fatalf("tele.NewBot(Offline): %v", err)
	}
	return tb
}

// sleepRecorder is a backoffPoller.sleep seam: it records every requested
// delay without sleeping and closes stop after limit sleeps.
type sleepRecorder struct {
	mu     sync.Mutex
	sleeps []time.Duration
	limit  int
	stop   chan struct{}
}

func (s *sleepRecorder) sleep(d time.Duration, _ <-chan struct{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sleeps = append(s.sleeps, d)
	if len(s.sleeps) >= s.limit {
		close(s.stop)
		return false
	}
	return true
}

// deterministicBackoff is defaultPollBackoff's exponential ladder without
// the jitter.
func deterministicBackoff(_ error, n int) time.Duration { return pollBackoffBase << uint(n-1) }

func durationsEqual(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// runPoll runs p.Poll synchronously against tb and fails the test unless it
// returns within timeout.
func runPoll(t *testing.T, p *backoffPoller, tb *tele.Bot, dest chan tele.Update, stop chan struct{}, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Poll(tb, dest, stop)
	}()
	waitReturned(t, done, timeout, "Poll")
}

// TestBackoffPollerBacksOffOnPersistentError (FIX-P-11): a getUpdates that
// keeps failing is retried after 1s, 2s, 4s, 8s, 16s - one request per
// sleep, never a hot loop.
func TestBackoffPollerBacksOffOnPersistentError(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusInternalServerError, `{"ok":false,"error_code":500,"description":"boom"}`)
	})
	stop := make(chan struct{})
	rec := &sleepRecorder{limit: 5, stop: stop}
	var errs []int
	p := newBackoffPoller(func(err error, n int) { errs = append(errs, n) }, nil)
	p.backoff = deterministicBackoff
	p.sleep = rec.sleep

	runPoll(t, p, newOfflineTeleBot(t, api.srv.URL, p), make(chan tele.Update, 1), stop, 2*time.Second)

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	if !durationsEqual(rec.sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", rec.sleeps, want)
	}
	if n := api.count("getUpdates"); n != 5 {
		t.Fatalf("getUpdates requests = %d, want 5 (one per backoff)", n)
	}
	if len(errs) != 5 || errs[4] != 5 {
		t.Fatalf("onError consecutive counts = %v, want [1 2 3 4 5]", errs)
	}
}

// TestBackoffPollerResetsAfterSuccess (FIX-P-11): one successful
// getUpdates resets the ladder and reports the recovery once.
func TestBackoffPollerResetsAfterSuccess(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		if n == 3 {
			writeAPI(w, http.StatusOK, apiNoUpdates)
			return
		}
		writeAPI(w, http.StatusInternalServerError, apiErr500)
	})
	stop := make(chan struct{})
	rec := &sleepRecorder{limit: 4, stop: stop}
	var recovered []int
	p := newBackoffPoller(nil, func(n int) { recovered = append(recovered, n) })
	p.backoff = deterministicBackoff
	p.sleep = rec.sleep

	runPoll(t, p, newOfflineTeleBot(t, api.srv.URL, p), make(chan tele.Update, 1), stop, 2*time.Second)

	want := []time.Duration{time.Second, 2 * time.Second, time.Second, 2 * time.Second}
	if !durationsEqual(rec.sleeps, want) {
		t.Fatalf("sleeps = %v, want %v (ladder reset after the success)", rec.sleeps, want)
	}
	if len(recovered) != 1 || recovered[0] != 2 {
		t.Fatalf("onSuccess calls = %v, want exactly [2]", recovered)
	}
}

// TestBackoffPollerHonorsFloodRetryAfter (FIX-P-11): a 429 waits exactly
// Telegram's retry_after (the real defaultPollBackoff - the flood branch
// carries no jitter).
func TestBackoffPollerHonorsFloodRetryAfter(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`)
	})
	stop := make(chan struct{})
	rec := &sleepRecorder{limit: 1, stop: stop}
	p := newBackoffPoller(nil, nil)
	p.sleep = rec.sleep

	runPoll(t, p, newOfflineTeleBot(t, api.srv.URL, p), make(chan tele.Update, 1), stop, 2*time.Second)

	if len(rec.sleeps) != 1 || rec.sleeps[0] != 7*time.Second {
		t.Fatalf("sleeps = %v, want [7s] (retry_after honored)", rec.sleeps)
	}
}

// TestBackoffPollerLongerCapOnUnauthorized (FIX-P-11): 401 (revoked
// token) and 409 (another poller) are branchable by code - 409 only via
// the parsed envelope, telebot's own error table has no 409 - and back off
// up to 5 minutes instead of 60s.
func TestBackoffPollerLongerCapOnUnauthorized(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
	}{
		{401, `{"ok":false,"error_code":401,"description":"Unauthorized"}`},
		{409, `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request; make sure that only one bot instance is running"}`},
	} {
		api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
			writeAPI(w, tc.code, tc.body)
		})
		_, err := getUpdates(newOfflineTeleBot(t, api.srv.URL, nil), 1, 0, time.Second, nil)
		var api409 *apiError
		if !errors.As(err, &api409) || api409.Code != tc.code {
			t.Fatalf("code %d: getUpdates err = %v, want an *apiError with Code %d", tc.code, err, tc.code)
		}
		for i := 0; i < 50; i++ {
			d := defaultPollBackoff(err, 20)
			if d < pollBackoffMax || d > pollBackoffMaxAuthFail*12/10 {
				t.Fatalf("code %d: defaultPollBackoff(err, 20) = %v, want within [%v, %v]", tc.code, d, pollBackoffMax, pollBackoffMaxAuthFail*12/10)
			}
		}
		if d := defaultPollBackoff(errors.New("dial tcp: no route to host"), 20); d > pollBackoffMax*11/10 {
			t.Fatalf("a transport error backs off %v, want <= %v (the 60s cap)", d, pollBackoffMax*11/10)
		}
	}
}

// TestDefaultPollBackoffLadderWithinJitter (FIX-P-11): the real backoff
// doubles from 1s, caps at 60s, and stays within +/-10% jitter.
func TestDefaultPollBackoffLadderWithinJitter(t *testing.T) {
	err := errors.New("transport")
	for n := 1; n <= 20; n++ {
		base := pollBackoffBase << uint(min(n-1, 12))
		if base > pollBackoffMax {
			base = pollBackoffMax
		}
		for i := 0; i < 20; i++ {
			d := defaultPollBackoff(err, n)
			if d < base-base/10 || d > base+base/10 {
				t.Fatalf("defaultPollBackoff(err, %d) = %v, want %v +/-10%%", n, d, base)
			}
		}
	}
}

// TestBackoffPollerStopDuringRequestLogsNoError (FIX-P-11): tele.Bot.Stop
// cancels the in-flight long-poll before it closes stop; that shutdown
// cancel is not a poll failure - no onError call, and Stop returns
// promptly instead of waiting for the long-poll to time out.
func TestBackoffPollerStopDuringRequestLogsNoError(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		longPollEmpty(w, r, 2*time.Second)
	})
	var mu sync.Mutex
	var errs []error
	p := newBackoffPoller(func(err error, n int) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}, nil)
	tb := newOfflineTeleBot(t, api.srv.URL, p)

	started := make(chan struct{})
	go func() {
		defer close(started)
		tb.Start()
	}()
	waitFor(t, 2*time.Second, "the first long-poll request", func() bool { return api.count("getUpdates") == 1 })
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tb.Stop()
	}()
	waitReturned(t, stopped, time.Second, "tele.Bot.Stop during an in-flight long-poll")
	waitReturned(t, started, time.Second, "tele.Bot.Start after Stop")

	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 0 {
		t.Fatalf("onError called %d time(s) on a clean stop: %v", len(errs), errs)
	}
}

// canceledTransport fails every request the way tele.Bot.Raw does once
// Bot.Stop has cancelled its request context.
type canceledTransport struct{}

func (canceledTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.Canceled
}

// TestBackoffPollerTreatsShutdownCancelAsNoFailure (FIX-P-11): the
// shutdown cancel can reach Poll BEFORE stop is closed (Bot.Stop closes
// its stop-client channel first); it is never counted, logged or allowed
// to flip the bot offline, and the loop just waits for stop.
func TestBackoffPollerTreatsShutdownCancelAsNoFailure(t *testing.T) {
	tb, err := tele.NewBot(tele.Settings{
		Offline: true, Token: "T", URL: "http://telegram.invalid",
		Client: &http.Client{Transport: canceledTransport{}},
	})
	if err != nil {
		t.Fatalf("tele.NewBot: %v", err)
	}
	stop := make(chan struct{})
	rec := &sleepRecorder{limit: 1, stop: stop}
	var errs []error
	p := newBackoffPoller(func(err error, n int) { errs = append(errs, err) }, nil)
	p.sleep = rec.sleep

	runPoll(t, p, tb, make(chan tele.Update, 1), stop, 2*time.Second)

	if len(errs) != 0 {
		t.Fatalf("onError called for a shutdown cancel: %v", errs)
	}
	if len(rec.sleeps) != 1 || rec.sleeps[0] != pollBackoffBase {
		t.Fatalf("sleeps = %v, want [%v] (wait for stop, no backoff ladder)", rec.sleeps, pollBackoffBase)
	}
}

// TestBackoffPollerStopsDuringBackoff (FIX-P-11): a long backoff sleep
// ends as soon as stop closes.
func TestBackoffPollerStopsDuringBackoff(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusInternalServerError, apiErr500)
	})
	p := newBackoffPoller(nil, nil)
	p.backoff = func(error, int) time.Duration { return time.Minute }
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Poll(newOfflineTeleBot(t, api.srv.URL, p), make(chan tele.Update, 1), stop)
	}()
	waitFor(t, 2*time.Second, "the failing request", func() bool { return api.count("getUpdates") == 1 })
	time.Sleep(50 * time.Millisecond)
	close(stop)
	waitReturned(t, done, 500*time.Millisecond, "Poll after stop during a 1-minute backoff")
	if n := api.count("getUpdates"); n != 1 {
		t.Fatalf("getUpdates requests = %d, want 1", n)
	}
}

// TestBackoffPollerDeliversUpdatesAndAdvancesOffset (FIX-P-11): updates
// reach dest and the next getUpdates asks for offset = last update_id + 1,
// with the same params telebot's own getUpdates sends.
func TestBackoffPollerDeliversUpdatesAndAdvancesOffset(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			writeAPI(w, http.StatusOK, `{"ok":true,"result":[{"update_id":41,"message":{"message_id":1,"date":1,"chat":{"id":1001,"type":"private"},"from":{"id":2002},"text":"hi"}}]}`)
			return
		}
		longPollEmpty(w, r, 20*time.Millisecond)
	})
	p := newBackoffPoller(nil, nil)
	stop := make(chan struct{})
	dest := make(chan tele.Update, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Poll(newOfflineTeleBot(t, api.srv.URL, p), dest, stop)
	}()
	select {
	case u := <-dest:
		if u.ID != 41 || u.Message == nil || u.Message.Text != "hi" {
			t.Fatalf("update = %+v, want update_id 41 with text hi", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no update delivered")
	}
	waitFor(t, 2*time.Second, "the second getUpdates", func() bool { return api.count("getUpdates") >= 2 })
	close(stop)
	waitReturned(t, done, 2*time.Second, "Poll after stop")

	api.mu.Lock()
	first, second := api.params["getUpdates"][0], api.params["getUpdates"][1]
	api.mu.Unlock()
	if first["offset"] != "1" || second["offset"] != "42" {
		t.Fatalf("offsets = %q then %q, want 1 then 42", first["offset"], second["offset"])
	}
	if second["timeout"] != "10" || second["allowed_updates"] != "null" {
		t.Fatalf("params = %v, want timeout=10 (longPollTimeout) and allowed_updates=null (telebot's own encoding)", second)
	}
}

// TestTelebotLongPollerHasNoBackoff documents WHY backoffPoller exists:
// telebot's own LongPoller retries a failing getUpdates with no delay at
// all. If a telebot upgrade adds backoff this fails - re-evaluate the
// custom poller then. Do not delete.
func TestTelebotLongPollerHasNoBackoff(t *testing.T) {
	api := newFakeBotAPI(t, func(method string, n int, w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusInternalServerError, apiErr500)
	})
	tb := newOfflineTeleBot(t, api.srv.URL, &tele.LongPoller{Timeout: time.Second})
	started := make(chan struct{})
	go func() {
		defer close(started)
		tb.Start()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for api.count("getUpdates") < 20 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	tb.Stop()
	waitReturned(t, started, 2*time.Second, "tele.Bot.Start after Stop")
	if n := api.count("getUpdates"); n < 20 {
		t.Fatalf("telebot LongPoller made %d getUpdates requests in 2s against a failing API, want >= 20 (no backoff) - if telebot gained backoff, revisit backoffPoller", n)
	}
}
