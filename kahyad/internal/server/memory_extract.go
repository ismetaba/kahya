// memory_extract.go implements POST /v1/memory/extract (`kahya memory
// extract`, G13 MEM-3): a manual run of the knowledge extractor
// (kahyad/internal/extract). A thin HTTP wrapper, like factengine.go: the
// route is a control route (G03 classifyRoute: anything not listed as a
// worker route is control), so a worker can never trigger it.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"kahya/kahyad/internal/extract"
	"kahya/kahyad/internal/factengine"
	"kahya/mcp/memory"
)

// memoryExtractTimeout bounds one synchronous /v1/memory/extract run (a
// whole nightly-size batch on the local model can take long; the CLI waits
// with no timeout of its own). Shutdown ends it sooner (boundTaskContext).
const memoryExtractTimeout = 2 * time.Hour

// MemoryExtractor is the extract.Runner surface the route needs -
// *extract.Runner satisfies it.
type MemoryExtractor interface {
	RunPending(ctx context.Context, traceID string) (extract.Stats, error)
	ExtractAll(ctx context.Context, traceID string) (extract.Stats, error)
	ExtractEpisode(ctx context.Context, traceID string, episodeID int64, prov factengine.Provenance) (extract.Stats, error)
	ExtractFile(ctx context.Context, traceID, relPath string) (extract.Stats, error)
}

var _ MemoryExtractor = (*extract.Runner)(nil)

// SetMemoryExtractor wires POST /v1/memory/extract to e. Call before
// Prepare(); the route answers 503 until it is set.
func (s *Server) SetMemoryExtractor(e MemoryExtractor) {
	s.memoryExtractor = e
}

// memoryExtractRequest is POST /v1/memory/extract's body: at most one of
// episode_id / file / all; none = one pending batch (the nightly job's
// RunPending).
type memoryExtractRequest struct {
	EpisodeID int64  `json:"episode_id"`
	File      string `json:"file"`
	All       bool   `json:"all"`
}

type memoryExtractResponse struct {
	extract.Stats
	TraceID string `json:"trace_id"`
}

func (s *Server) handleMemoryExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.memoryExtractor == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory extraction not available")
		return
	}
	var req memoryExtractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	selectors := 0
	if req.EpisodeID != 0 {
		selectors++
	}
	if req.File != "" {
		selectors++
	}
	if req.All {
		selectors++
	}
	if selectors > 1 || req.EpisodeID < 0 {
		writeJSONError(w, http.StatusBadRequest, "give at most one of episode_id, file, all")
		return
	}
	relPath := ""
	if req.File != "" {
		if relPath = extract.NormalizeRelPath(s.cfg.MemoryDir, req.File); relPath == "" {
			writeJSONError(w, http.StatusBadRequest, "file must be a note path inside the memory dir")
			return
		}
		// FIX-FINAL memory-confirm-nfd-duplicate-episode: ExtractFile looks
		// the episode up by its exact source_path bytes; a case / NFD
		// variant of an existing note's path is resolved to the name the
		// volume stores (a path that does not exist keeps its spelling, so
		// ExtractFile still answers 404).
		if _, lerr := os.Lstat(filepath.Join(s.cfg.MemoryDir, filepath.FromSlash(relPath))); lerr == nil {
			stored, err := memory.StoredRelPath(s.cfg.MemoryDir, relPath)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "resolve note path: "+err.Error())
				return
			}
			relPath = stored
		}
	}

	if !s.inflight.begin() {
		writeJSONError(w, http.StatusServiceUnavailable, "kahyad is shutting down")
		return
	}
	defer s.inflight.end()
	ctx, cancel := s.boundTaskContext(r.Context(), memoryExtractTimeout)
	defer cancel()

	traceID := traceIDFromContext(r)
	var (
		st  extract.Stats
		err error
	)
	switch {
	case req.EpisodeID != 0:
		st, err = s.memoryExtractor.ExtractEpisode(ctx, traceID, req.EpisodeID, factengine.ProvenanceAgentDerived)
	case relPath != "":
		st, err = s.memoryExtractor.ExtractFile(ctx, traceID, relPath)
	case req.All:
		st, err = s.memoryExtractor.ExtractAll(ctx, traceID)
	default:
		st, err = s.memoryExtractor.RunPending(ctx, traceID)
	}
	switch {
	case errors.Is(err, extract.ErrInProgress):
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, extract.ErrEpisodeNotFound):
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		s.log.With(traceID).Error("memory_extract_failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(memoryExtractResponse{Stats: st, TraceID: traceID})
}
