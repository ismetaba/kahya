// ingest.go implements POST /v1/ingest (`kahya ingest <dosya|->`, G13
// MEM-6 phase 2): a thin HTTP wrapper over kahyad/internal/ingest.Runner.
// It is a control route (G03 classifyRoute: not listed as a worker route)
// - choosing to remember an outside document is the user's trust decision,
// so a model can never call it, and it is not an MCP tool.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"kahya/kahyad/internal/ingest"
	"kahya/mcp/memory"
)

// ingestTimeout bounds one synchronous ingest (a long document on the local
// model can take minutes; the CLI waits with no timeout of its own).
// Shutdown ends it sooner (boundTaskContext).
const ingestTimeout = 30 * time.Minute

// Ingester is the ingest.Runner surface the route needs.
type Ingester interface {
	Ingest(ctx context.Context, traceID string, req ingest.Request) (ingest.Result, error)
	MaxBytes() int64
}

var _ Ingester = (*ingest.Runner)(nil)

// SetIngester wires POST /v1/ingest. Call before Prepare(); the route
// answers 503 until it is set.
func (s *Server) SetIngester(i Ingester) { s.ingester = i }

// MemoryNoteWriter is the memory-repo writer kahya ingest uses: it resolves
// the process-wide memory server (memoryServer) at call time, so ingest
// notes are written under the same mutex as memory_write and memory confirm
// - main.go builds the ingest runner before Prepare, when that server does
// not exist yet.
func (s *Server) MemoryNoteWriter() ingest.NoteWriter { return memoryNoteWriter{s: s} }

type memoryNoteWriter struct{ s *Server }

func (w memoryNoteWriter) HandleWriteTiered(ctx context.Context, traceID string, args memory.TieredWriteArgs) (memory.TieredWriteOutput, error) {
	m := w.s.memoryServer()
	if m == nil {
		return memory.TieredWriteOutput{}, errors.New("server: memory repo not wired (SetMCPMemory)")
	}
	return m.HandleWriteTiered(ctx, traceID, args)
}

// HandleForget retracts a superseded interrupted ingest note (FIX-FINAL
// ingest-superseded-orphan-note-stays-active) through the same memory
// server and mutex.
func (w memoryNoteWriter) HandleForget(ctx context.Context, traceID string, args memory.MemoryForgetArgs) (memory.MemoryForgetOutput, error) {
	m := w.s.memoryServer()
	if m == nil {
		return memory.MemoryForgetOutput{}, errors.New("server: memory repo not wired (SetMCPMemory)")
	}
	return m.HandleForget(ctx, traceID, args)
}

// ingestRequest is POST /v1/ingest's body.
type ingestRequest struct {
	ContentB64 string `json:"content_b64"`
	SourceRef  string `json:"source_ref"`
	// SourcePaths are the absolute paths the CLI read the file from (as
	// given + symlink-resolved); matched against secret_lane_globs before
	// the Reader runs (ingest.Request.SourcePaths).
	SourcePaths []string `json:"source_paths"`
	Title       string   `json:"title"`
	LaneHint    string   `json:"lane_hint"`
	NoRaw       bool     `json:"no_raw"`
}

type ingestResponse struct {
	OK bool `json:"ok"`
	ingest.Result
	TraceID string `json:"trace_id"`
}

// ingestErrorResponse carries the Turkish text, the reason code and, for a
// 413, the configured cap.
type ingestErrorResponse struct {
	Error    string `json:"error"`
	Reason   string `json:"reason,omitempty"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
}

func writeIngestError(w http.ResponseWriter, status int, resp ingestErrorResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// ingestStatus maps an ingest failure reason onto an HTTP status.
func ingestStatus(reason string) int {
	switch reason {
	case ingest.ReasonBinary, ingest.ReasonEmpty, ingest.ReasonBadRequest:
		return http.StatusBadRequest
	case ingest.ReasonTooLarge:
		return http.StatusRequestEntityTooLarge
	case ingest.ReasonDuplicate, ingest.ReasonInProgress:
		return http.StatusConflict
	case ingest.ReasonLocalUnavailable:
		return http.StatusServiceUnavailable
	case ingest.ReasonRejected:
		return http.StatusUnprocessableEntity
	case ingest.ReasonCloudFailed, ingest.ReasonLocalFailed:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if s.ingester == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ingest not available")
		return
	}
	maxBytes := s.ingester.MaxBytes()
	tooLarge := func() {
		writeIngestError(w, http.StatusRequestEntityTooLarge, ingestErrorResponse{
			Error: fmt.Sprintf(ingest.MsgIngestTooLargeFmt, maxBytes), Reason: ingest.ReasonTooLarge, MaxBytes: maxBytes,
		})
	}
	// The body is the base64 document plus small labels: refuse it before
	// reading more than that.
	limit := int64(base64.StdEncoding.EncodedLen(int(maxBytes))) + 64<<10
	var req ingestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			tooLarge()
			return
		}
		if !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	content, err := base64.StdEncoding.DecodeString(req.ContentB64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "content_b64 must be standard base64")
		return
	}
	if int64(len(content)) > maxBytes {
		tooLarge()
		return
	}

	if !s.inflight.begin() {
		writeJSONError(w, http.StatusServiceUnavailable, "kahyad is shutting down")
		return
	}
	defer s.inflight.end()
	ctx, cancel := s.boundTaskContext(r.Context(), ingestTimeout)
	defer cancel()

	traceID := traceIDFromContext(r)
	res, err := s.ingester.Ingest(ctx, traceID, ingest.Request{
		Content: content, SourceRef: req.SourceRef, SourcePaths: req.SourcePaths, Title: req.Title, LaneHint: req.LaneHint, NoRaw: req.NoRaw,
	})
	if err != nil {
		var ie *ingest.Error
		if errors.As(err, &ie) {
			writeIngestError(w, ingestStatus(ie.Reason), ingestErrorResponse{Error: ie.Error(), Reason: ie.Reason, MaxBytes: ie.MaxBytes})
			return
		}
		s.log.With(traceID).Error("ingest_failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ingestResponse{OK: true, Result: res, TraceID: traceID})
}
