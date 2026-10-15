// memory_confirm.go implements POST /v1/memory/confirm (`kahya memory
// confirm <yol>`, G13 MEM-5 phase 2) and owns the process-wide
// mcp/memory.Server every memory-repo write goes through.
//
// The route is the human gate on an agent-written note (HANDOFF §5 memory
// #1): it promotes the note's front matter to user_asserted (mcp/memory
// HandleConfirm), so its chunks pass the reflex-injection tier filter. It
// is a control route (G03 classifyRoute: not listed as a worker route), so
// a worker can never call it, and HandleConfirm is never registered as an
// MCP tool - a model cannot lift its own quarantine.
package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"kahya/kahyad/internal/extract"
	"kahya/mcp/memory"
)

// LockMemoryRepo takes the memory-repo mutex of the ONE mcp/memory.Server
// (memoryServer) and returns its unlock - consolidation.RepoLocker, so the
// nightly author=user pre-commit and merge never interleave with a memory
// tool's write-then-commit (FIX-FINAL precommit-claims-kahyad-written-
// note-as-user). The server is resolved at lock time, after every Set*
// wiring. With no memory server there is no concurrent writer: a no-op.
func (s *Server) LockMemoryRepo() func() {
	m := s.memoryServer()
	if m == nil {
		return func() {}
	}
	return m.LockRepo()
}

// memoryServer returns the ONE mcp/memory.Server this kahyad process writes
// the memory repo through: the /v1/mcp memory tools (buildMCPHandler) and
// the control routes (memory confirm; kahya ingest through
// MemoryNoteWriter) share it, so its mutex serializes every memory-repo
// mutation (file write + git index + commit + reindex) across them - two
// instances would race .git/index.lock. It is built once, on first use,
// from the wiring the Set* calls made (all before Prepare in main.go); nil
// until SetMCPMemory wired a memory dir and an indexer.
func (s *Server) memoryServer() *memory.Server {
	s.memSrvOnce.Do(func() {
		if s.mcpMemoryDir == "" || s.mcpIndexer == nil {
			return
		}
		var searcher memory.Searcher
		if s.search != nil {
			searcher = mcpSearchAdapter{inner: s.search, lane: s.memLane}
		}
		m := memory.New(s.mcpMemoryDir, searcher, s.mcpIndexer, s.eventLogger, s.log)
		// FIX-U-1: the default inbox day file follows the configured calendar
		// zone (SetLocation, cfg.Location()).
		m.Location = s.location()
		// FIX-U-3: memory_write classifies its content locally before any byte
		// lands; a secret hit is redirected under the secret-lane memory dir
		// and escalates the attributed task's lane (memory_lane.go). Ingest
		// (MEM-6) reuses the seam's dir and glob coverage check.
		m.SetSecretLaneSeam(s.mcpSecretLane)
		// FIX-MEM-4: entity_lookup / fact_query read the graph layer, always
		// in the cloud lane (mcpGraphAdapter). Unwired = ErrGraphUnavailable.
		if s.graph != nil {
			m.SetGraph(mcpGraphAdapter{g: s.graph})
		}
		s.memSrv = m
	})
	return s.memSrv
}

// memoryConfirmRequest is POST /v1/memory/confirm's body: {"file": "<note
// path, relative to the memory dir or absolute inside it>"}.
type memoryConfirmRequest struct {
	File string `json:"file"`
}

type memoryConfirmResponse struct {
	OK bool `json:"ok"`
	memory.MemoryConfirmOutput
	TraceID string `json:"trace_id"`
}

// handleMemoryConfirm answers 200 with the promoted note, 400 for a path
// outside the memory dir, 404 for no such note, 409 when the note is
// already at a user tier, 503 unwired.
func (s *Server) handleMemoryConfirm(w http.ResponseWriter, r *http.Request) {
	m := s.memoryServer()
	if m == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "memory confirm not available")
		return
	}
	var req memoryConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	relPath := extract.NormalizeRelPath(s.mcpMemoryDir, req.File)
	if relPath == "" {
		writeJSONError(w, http.StatusBadRequest, "file must be a note path inside the memory dir")
		return
	}
	traceID := traceIDFromContext(r)
	out, err := m.HandleConfirm(r.Context(), traceID, memory.MemoryConfirmArgs{File: relPath})
	switch {
	case errors.Is(err, memory.ErrNotePath):
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, memory.ErrNoteNotFound):
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, memory.ErrNoteAlreadyConfirmed):
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.log.With(traceID).Error("memory_confirm_failed", "file", relPath, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(memoryConfirmResponse{OK: true, MemoryConfirmOutput: out, TraceID: traceID})
}
