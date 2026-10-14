package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	tele "gopkg.in/telebot.v4"
)

// Long-poll retry bounds (FIX-P-11).
const (
	pollBackoffBase        = 1 * time.Second
	pollBackoffMax         = 60 * time.Second
	pollBackoffMaxAuthFail = 5 * time.Minute // 401/409: nothing to gain by hammering
	offlineAfterPollErrors = 3               // consecutive failures => Bot.online=false
)

// backoffPoller is kahyad's LONG-POLLING-ONLY tele.Poller (FIX-P-11).
// telebot's own LongPoller.Poll (v4.0.0-beta.10) retries a failed
// getUpdates with ZERO delay and logs only under Verbose, so a persistent
// error (network down, revoked token, a second poller) became a silent
// hot loop. This one keeps the exact getUpdates wire call (over the
// exported Bot.Raw) but sleeps with capped exponential backoff + jitter
// between failures, honors Telegram's 429 retry_after, backs off longer on
// 401/409, reports every failure (onError) and the recovery (onSuccess) so
// the Bot can log them as JSONL and track Online. Still no webhook and no
// listen address (TestBuildSettingsLongPollOnly).
type backoffPoller struct {
	Timeout        time.Duration
	Limit          int
	AllowedUpdates []string
	lastUpdateID   int

	onError   func(err error, consecutive int) // may be nil
	onSuccess func(consecutive int)            // after >=1 failure; may be nil
	backoff   func(err error, consecutive int) time.Duration
	sleep     func(d time.Duration, stop <-chan struct{}) bool // false => stop closed
}

func newBackoffPoller(onError func(error, int), onSuccess func(int)) *backoffPoller {
	return &backoffPoller{
		Timeout: longPollTimeout, onError: onError, onSuccess: onSuccess,
		backoff: defaultPollBackoff, sleep: sleepOrStop,
	}
}

// apiError is the Bot API's own {"ok":false,"error_code":N,...} envelope,
// parsed by this package from the data Raw returns alongside its error:
// telebot maps only the descriptions it knows to *tele.Error, and a 409
// "Conflict: terminated by other getUpdates request" falls to a plain
// fmt.Errorf with no code at all, so errors.As(*tele.Error) could never
// see it. Parsing the envelope covers 401, 409 and 429 uniformly.
type apiError struct {
	Code        int
	Description string
	RetryAfter  int // seconds; 429 only
}

func (e *apiError) Error() string { return fmt.Sprintf("telegram: %s (%d)", e.Description, e.Code) }

// parseAPIError returns the envelope error carried in data, or nil when
// data is not an API error envelope.
func parseAPIError(data []byte) *apiError {
	var env struct {
		Ok          bool           `json:"ok"`
		Code        int            `json:"error_code"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}
	if json.Unmarshal(data, &env) != nil || env.Ok || env.Code == 0 {
		return nil
	}
	e := &apiError{Code: env.Code, Description: env.Description}
	if ra, ok := env.Parameters["retry_after"].(float64); ok {
		e.RetryAfter = int(ra)
	}
	return e
}

// defaultPollBackoff is the delay after the consecutive-th failure in a
// row: Telegram's own retry_after for a 429, else 1s, 2s, 4s, ... capped at
// 60s (5 min for 401/409), with +/-10% jitter.
func defaultPollBackoff(err error, consecutive int) time.Duration {
	var api *apiError
	isAPI := errors.As(err, &api)
	if isAPI && api.Code == 429 && api.RetryAfter > 0 {
		return time.Duration(api.RetryAfter) * time.Second
	}
	maxD := pollBackoffMax
	if isAPI && (api.Code == 401 || api.Code == 409) {
		maxD = pollBackoffMaxAuthFail
	}
	d := pollBackoffBase << uint(min(max(consecutive-1, 0), 12))
	if d > maxD {
		d = maxD
	}
	return d + time.Duration(rand.Int64N(int64(d)/5)) - d/10
}

// sleepOrStop waits d, or returns false at once when stop closes.
func sleepOrStop(d time.Duration, stop <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return false
	case <-t.C:
		return true
	}
}

// Poll implements tele.Poller.
func (p *backoffPoller) Poll(b *tele.Bot, dest chan tele.Update, stop chan struct{}) {
	consecutive := 0
	for {
		select {
		case <-stop:
			return
		default:
		}
		updates, err := getUpdates(b, p.lastUpdateID+1, p.Limit, p.Timeout, p.AllowedUpdates)
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			// tele.Bot.Stop cancels the in-flight request (its stop-client
			// channel) BEFORE it closes stop, so a clean shutdown surfaces
			// here as context.Canceled first: Raw's request context is
			// cancelled by nothing else. Not a failure - no log, no
			// offline flip; wait for stop.
			if errors.Is(err, context.Canceled) {
				if !p.sleep(pollBackoffBase, stop) {
					return
				}
				continue
			}
			consecutive++
			if p.onError != nil {
				p.onError(err, consecutive)
			}
			if !p.sleep(p.backoff(err, consecutive), stop) {
				return
			}
			continue
		}
		if consecutive > 0 && p.onSuccess != nil {
			p.onSuccess(consecutive)
		}
		consecutive = 0
		for _, u := range updates {
			p.lastUpdateID = u.ID
			select {
			case dest <- u:
			case <-stop:
				return
			}
		}
	}
}

// getUpdates mirrors telebot's unexported Bot.getUpdates (v4.0.0-beta.10
// bot_raw.go) byte for byte - same params, same allowed_updates JSON
// (nil => "null", Telegram's default set) - over the exported Raw, but
// returns the parsed *apiError for an API error envelope so backoff can
// branch on the numeric code.
func getUpdates(b *tele.Bot, offset, limit int, timeout time.Duration, allowed []string) ([]tele.Update, error) {
	params := map[string]string{
		"offset":  strconv.Itoa(offset),
		"timeout": strconv.Itoa(int(timeout / time.Second)),
	}
	data, _ := json.Marshal(allowed)
	params["allowed_updates"] = string(data)
	if limit != 0 {
		params["limit"] = strconv.Itoa(limit)
	}
	raw, err := b.Raw("getUpdates", params)
	if err != nil {
		if api := parseAPIError(raw); api != nil {
			return nil, api
		}
		return nil, err
	}
	var resp struct{ Result []tele.Update }
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}
