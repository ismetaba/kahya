// remembered.go implements W5-03's remembered-moment marking flow: POST
// /v1/remembered {trace_id, channel}, a plain, narrow HTTP <-> Go wrapper
// over kahyad/internal/remembered.Marker - exactly mirroring
// factengine.go's identical "thin HTTP wrapper" shape. `kahya remembered
// --trace <id>` (channel="local") is this route's CLI client; the
// Telegram "🌟 Hatırladı" callback (kahyad/internal/telegram) calls the
// SAME Marker directly in-process instead (never a second HTTP hop for a
// send this bot already made) - both are thin callers of the one
// remembered-moment write path (kahyad is brain.db's only writer).
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// RememberedMarker is the narrow kahyad/internal/remembered.Marker
// surface this route needs - *remembered.Marker satisfies it directly,
// with no adapter.
type RememberedMarker interface {
	Mark(ctx context.Context, traceID, channel string) (duplicate bool, err error)
}

// SetRememberedMarker wires POST /v1/remembered to m. Call before
// Prepare(); the route answers 503 until this is set, matching this
// package's existing "unwired dependency" convention (SetFactEngine et
// al.).
func (s *Server) SetRememberedMarker(m RememberedMarker) {
	s.remembered = m
}

// rememberedRequest is POST /v1/remembered's body. channel defaults to
// "local" when absent/empty (every CLI caller - `kahya remembered --trace
// <id>` - IS the local surface; the Telegram callback always passes
// "remote" explicitly).
type rememberedRequest struct {
	TraceID string `json:"trace_id"`
	Channel string `json:"channel"`
}

type rememberedResponse struct {
	OK        bool   `json:"ok"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Error     string `json:"error,omitempty"`
}

// handleRemembered implements `kahya remembered --trace <id>` (and the
// Telegram "Hatırladı" button's in-process counterpart, though that path
// calls kahyad/internal/remembered.Marker directly rather than looping
// back through this HTTP route). A trace_id with no events row at all
// answers 422 with remembered.ErrUnknownTrace's own Turkish message,
// printed by the CLI verbatim (task spec: unknown trace ⇒ Turkish error +
// non-zero exit).
func (s *Server) handleRemembered(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.remembered == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "remembered marker not available")
		return
	}
	var req rememberedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.TraceID) == "" {
		writeJSONError(w, http.StatusBadRequest, "trace_id is required")
		return
	}
	channel := req.Channel
	if channel == "" {
		channel = "local"
	}

	duplicate, err := s.remembered.Mark(r.Context(), req.TraceID, channel)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(rememberedResponse{OK: false, Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(rememberedResponse{OK: true, Duplicate: duplicate})
}
