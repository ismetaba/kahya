// Package faketelegram implements a minimal, in-process fake of the
// Telegram Bot API HTTP surface (W3-10, the tests/w3 gate suite): getMe,
// getUpdates (real long-polling semantics), sendMessage, editMessageText,
// and answerCallbackQuery. It exists so the gate tests can boot a REAL
// child kahyad process (kahyad/internal/telegram is under kahyad/internal/
// and therefore not importable from this package — Go's internal-package
// boundary) with a REAL gopkg.in/telebot.v4 client talking to a fake
// transport instead of the real api.telegram.org, with no live BotFather
// token anywhere.
//
// kahyad/internal/telegram.Config.APIURL (W3-10's own small addition to
// that package) points the real *telebot.Bot at this server's URL; a dev-
// only KAHYA_TELEGRAM_TOKEN_OVERRIDE substitutes for the real Keychain
// token (both wired in kahyad/main.go, active only under KAHYA_ENV=dev).
//
// This is a wire-protocol fake only (mirrors tests/e2e/mockanthropic's own
// framing doc comment): every call is recorded verbatim so a test can
// assert on exactly what kahyad's real telegram package sent, including
// the byte-exact WYSIWYE diff text and the presence/absence of an inline
// keyboard.
package faketelegram

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Button is one inline keyboard button telebot's InlineButton marshals to
// (only the two fields kahyad/internal/telegram/approvals.go's
// approvalMarkup ever sets).
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// SentMessage is one recorded sendMessage call.
type SentMessage struct {
	ChatID  int64
	Text    string
	Buttons []Button // nil/empty when the message carried no reply_markup keyboard
}

// HasKeyboard reports whether this message carried an inline keyboard.
func (m SentMessage) HasKeyboard() bool { return len(m.Buttons) > 0 }

// EditedMessage is one recorded editMessageText call.
type EditedMessage struct {
	ChatID    int64
	MessageID int
	Text      string
}

// queuedUpdate is one PushCallback-enqueued update, kept in arrival order;
// ID is the synthetic "update_id" telebot's LongPoller tracks as its own
// offset.
type queuedUpdate struct {
	ID  int
	Raw json.RawMessage
}

// Server is the fake Telegram Bot API. Construct with New; call Close when
// done (typically via t.Cleanup).
type Server struct {
	srv *httptest.Server

	mu        sync.Mutex
	sent      []SentMessage
	edited    []EditedMessage
	responded []string
	nextMsgID int

	updates   []queuedUpdate
	updateSeq int
	notify    chan struct{} // closed + replaced on every push (broadcast-wakeup idiom)
}

// New starts a new fake Telegram Bot API server bound to an ephemeral
// 127.0.0.1 port.
func New() *Server {
	s := &Server{notify: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(s.route))
	return s
}

// URL is this fake's base URL, suitable for
// kahyad/internal/telegram.Config.APIURL / config.Config.TelegramAPIURL.
func (s *Server) URL() string { return s.srv.URL }

// Close shuts the fake server down.
func (s *Server) Close() { s.srv.Close() }

// SentMessages returns every sendMessage call recorded so far, in arrival
// order.
func (s *Server) SentMessages() []SentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SentMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

// EditedMessages returns every editMessageText call recorded so far.
func (s *Server) EditedMessages() []EditedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EditedMessage, len(s.edited))
	copy(out, s.edited)
	return out
}

// Responded returns every answerCallbackQuery response text recorded so
// far.
func (s *Server) Responded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.responded))
	copy(out, s.responded)
	return out
}

// PushCallback enqueues a synthetic inbound callback_query update (an
// inline-button tap, real OR forged — this fake makes no distinction; the
// REAL kahyad/internal/policy.Engine.Approve's own surface="local" backstop
// is what actually rejects a W3 approval attempt regardless of how
// convincingly it was delivered) from (chatID, userID) with the given raw
// callback_data — the real child kahyad's long-poller will pick this up on
// its next getUpdates round-trip (bounded by kahyad/internal/telegram's own
// longPollTimeout, 10s).
func (s *Server) PushCallback(chatID, userID int64, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateSeq++
	id := s.updateSeq
	upd := map[string]any{
		"update_id": id,
		"callback_query": map[string]any{
			"id":   fmt.Sprintf("cbq-%d", id),
			"from": map[string]any{"id": userID, "is_bot": false, "first_name": "Kahya"},
			"message": map[string]any{
				"message_id": 1,
				"date":       time.Now().Unix(),
				"chat":       map[string]any{"id": chatID, "type": "private"},
			},
			"data":          data,
			"chat_instance": "kahya-w3-gate-test",
		},
	}
	raw, _ := json.Marshal(upd)
	s.updates = append(s.updates, queuedUpdate{ID: id, Raw: raw})
	s.wakeLocked()
}

// wakeLocked broadcasts to every blocked getUpdates call. Must be called
// with s.mu held.
func (s *Server) wakeLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}

// route dispatches "/bot<token>/<method>" to the matching handler — the
// exact URL shape gopkg.in/telebot.v4's Bot.Raw builds
// (b.URL + "/bot" + b.Token + "/" + method).
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "bot") {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "getMe":
		s.handleGetMe(w, r)
	case "getUpdates":
		s.handleGetUpdates(w, r)
	case "sendMessage":
		s.handleSendMessage(w, r)
	case "editMessageText":
		s.handleEditMessageText(w, r)
	case "answerCallbackQuery":
		s.handleAnswerCallbackQuery(w, r)
	default:
		// Every other method (setMyCommands, deleteMessage, ...) this bot
		// might incidentally call: a harmless, always-ok no-op response —
		// this fake only needs to model the methods kahyad/internal/
		// telegram actually calls (this file's own doc comment).
		writeResult(w, true)
	}
}

func writeResult(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func decodeStringParams(r *http.Request) map[string]string {
	var params map[string]string
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &params)
	if params == nil {
		params = map[string]string{}
	}
	return params
}

func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	writeResult(w, map[string]any{
		"id": 1, "is_bot": true, "first_name": "KahyaGateTestBot", "username": "kahya_w3_gate_test_bot",
	})
}

// handleGetUpdates implements REAL long-polling: it blocks until either a
// queued update with id >= offset exists, or the requested timeout
// elapses, whichever comes first — never busy-spinning (telebot's own
// LongPoller.Poll retries with NO backoff on an empty-but-successful
// response, so a fast, correct empty-array reply after the honored
// timeout is what keeps this from pegging a CPU core for the test's whole
// runtime).
func (s *Server) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	params := decodeStringParams(r)
	offset, _ := strconv.Atoi(params["offset"])
	timeoutS, _ := strconv.Atoi(params["timeout"])
	deadline := time.Now().Add(time.Duration(timeoutS) * time.Second)

	for {
		s.mu.Lock()
		var match []json.RawMessage
		for _, u := range s.updates {
			if u.ID >= offset {
				match = append(match, u.Raw)
			}
		}
		ch := s.notify
		s.mu.Unlock()

		if len(match) > 0 {
			respondUpdates(w, match)
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			respondUpdates(w, nil)
			return
		}
		select {
		case <-ch:
			continue
		case <-time.After(remaining):
			respondUpdates(w, nil)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func respondUpdates(w http.ResponseWriter, raws []json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	var b strings.Builder
	b.WriteString(`{"ok":true,"result":[`)
	for i, r := range raws {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(r)
	}
	b.WriteString(`]}`)
	_, _ = w.Write([]byte(b.String()))
}

// replyMarkupJSON mirrors telebot's own ReplyMarkup shape narrowly (just
// enough to recover the inline keyboard's buttons — see
// gopkg.in/telebot.v4's options.go embedSendOptions: the SendMessage
// params map's "reply_markup" VALUE is itself a JSON-encoded STRING, not a
// nested object, so callers must json.Unmarshal it a second time).
type replyMarkupJSON struct {
	InlineKeyboard [][]Button `json:"inline_keyboard"`
}

func parseButtons(replyMarkupParam string) []Button {
	if replyMarkupParam == "" {
		return nil
	}
	var rm replyMarkupJSON
	if err := json.Unmarshal([]byte(replyMarkupParam), &rm); err != nil {
		return nil
	}
	var out []Button
	for _, row := range rm.InlineKeyboard {
		out = append(out, row...)
	}
	return out
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	params := decodeStringParams(r)
	chatID, _ := strconv.ParseInt(params["chat_id"], 10, 64)
	text := params["text"]
	buttons := parseButtons(params["reply_markup"])

	s.mu.Lock()
	s.nextMsgID++
	msgID := s.nextMsgID
	s.sent = append(s.sent, SentMessage{ChatID: chatID, Text: text, Buttons: buttons})
	s.mu.Unlock()

	writeResult(w, map[string]any{
		"message_id": msgID,
		"date":       time.Now().Unix(),
		"chat":       map[string]any{"id": chatID, "type": "private"},
		"text":       text,
	})
}

func (s *Server) handleEditMessageText(w http.ResponseWriter, r *http.Request) {
	params := decodeStringParams(r)
	chatID, _ := strconv.ParseInt(params["chat_id"], 10, 64)
	msgID, _ := strconv.Atoi(params["message_id"])
	text := params["text"]

	s.mu.Lock()
	s.edited = append(s.edited, EditedMessage{ChatID: chatID, MessageID: msgID, Text: text})
	s.mu.Unlock()

	writeResult(w, map[string]any{
		"message_id": msgID,
		"date":       time.Now().Unix(),
		"chat":       map[string]any{"id": chatID, "type": "private"},
		"text":       text,
	})
}

func (s *Server) handleAnswerCallbackQuery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)

	s.mu.Lock()
	s.responded = append(s.responded, body.Text)
	s.mu.Unlock()

	writeResult(w, true)
}
