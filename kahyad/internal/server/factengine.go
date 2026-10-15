// factengine.go implements the W5-04 fact/entity control routes: POST
// /v1/fact/confirm (`kahya fact confirm <id>`), POST /v1/fact/retract
// (`kahya fact retract <id>`), POST /v1/entity/merge (`kahya entity merge
// <a> <b> --evidence <fact_id>`), POST /v1/entity/split (`kahya entity
// split <merge_ledger_id>`). All four are plain, narrow HTTP <-> Go
// wrappers over kahyad/internal/factengine.Engine - exactly mirroring
// consolidation.go's identical "thin HTTP wrapper" shape (that file's own
// doc comment). G13 MEM-2 phase 2 adds the read side: GET /v1/entity/list
// (`kahya entity list [<ad>] [--provisional]`) and GET /v1/entity/show/{id}
// (`kahya entity show <id>`) - "show/" because a method-restricted
// "/v1/entity/{id}" conflicts with the any-method /v1/entity/merge|split
// patterns in Go's ServeMux. All are control routes (G03).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"kahya/kahyad/internal/factengine"
)

// FactEngineRunner is the narrow kahyad/internal/factengine.Engine
// surface these routes need - *factengine.Engine satisfies it directly,
// with no adapter.
type FactEngineRunner interface {
	ConfirmFact(ctx context.Context, traceID string, factID int64) error
	RetractFact(ctx context.Context, traceID, subject, predicate, object, sessionID string) (int64, error)
	MergeEntities(ctx context.Context, traceID string, dstEntityID, srcEntityID, evidenceFactID int64, actor string) (int64, error)
	SplitEntities(ctx context.Context, traceID string, mergeLedgerID int64, actor string) (int64, error)
}

// SetFactEngine wires the four /v1/fact* and /v1/entity* routes to e.
// Call before Prepare(); every route answers 503 until this is set,
// matching this package's existing "unwired dependency" convention
// (SetConsolidation/SetSearcher/...).
func (s *Server) SetFactEngine(e FactEngineRunner) {
	s.factEngine = e
}

type factActionResponse struct {
	OK    bool   `json:"ok"`
	ID    int64  `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

// factConfirmRequest is POST /v1/fact/confirm's body: {"fact_id": <id>}.
type factConfirmRequest struct {
	FactID int64 `json:"fact_id"`
}

// handleFactConfirm implements `kahya fact confirm <id>` (lifts the
// agent_derived quarantine half of factengine.InjectionEligible - see
// that predicate's own doc comment).
func (s *Server) handleFactConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.factEngine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "fact engine not available")
		return
	}
	var req factConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FactID == 0 {
		writeJSONError(w, http.StatusBadRequest, "fact_id is required")
		return
	}
	traceID := traceIDFromContext(r)
	if err := s.factEngine.ConfirmFact(r.Context(), traceID, req.FactID); err != nil {
		writeFactActionError(w, err)
		return
	}
	writeFactActionOK(w, req.FactID)
}

// factRetractRequest is POST /v1/fact/retract's body: the (subject,
// predicate, object) triple identifying the ACTIVE fact to close, plus
// the session_id the retraction utterance came from (negative evidence
// is attributed to it, same as any other evidence row).
type factRetractRequest struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
	SessionID string `json:"session_id"`
}

// handleFactRetract implements `kahya fact retract <id>` - the CLI
// resolves the fact id to its (subject, predicate, object) triple first
// (GET-then-POST, mirroring runApprove's "show the target before acting"
// shape) so this route's contract stays symmetric with
// factengine.RetractFact's own triple-keyed signature.
func (s *Server) handleFactRetract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.factEngine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "fact engine not available")
		return
	}
	var req factRetractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Subject == "" || req.Predicate == "" || req.Object == "" {
		writeJSONError(w, http.StatusBadRequest, "subject/predicate/object are required")
		return
	}
	traceID := traceIDFromContext(r)
	factID, err := s.factEngine.RetractFact(r.Context(), traceID, req.Subject, req.Predicate, req.Object, req.SessionID)
	if err != nil {
		writeFactActionError(w, err)
		return
	}
	writeFactActionOK(w, factID)
}

// entityMergeRequest is POST /v1/entity/merge's body: `kahya entity merge
// <a> <b> --evidence <fact_id>` merges b (src, loses) into a (dst,
// survives) - evidence_fact_id MUST name a real, existing fact
// (factengine.ErrMergeRequiresEvidence otherwise).
type entityMergeRequest struct {
	DstEntityID    int64  `json:"dst_entity_id"`
	SrcEntityID    int64  `json:"src_entity_id"`
	EvidenceFactID int64  `json:"evidence_fact_id"`
	Actor          string `json:"actor"`
}

func (s *Server) handleEntityMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.factEngine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "fact engine not available")
		return
	}
	var req entityMergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DstEntityID == 0 || req.SrcEntityID == 0 {
		writeJSONError(w, http.StatusBadRequest, "dst_entity_id/src_entity_id are required")
		return
	}
	actor := req.Actor
	if actor == "" {
		actor = "user"
	}
	traceID := traceIDFromContext(r)
	mergeID, err := s.factEngine.MergeEntities(r.Context(), traceID, req.DstEntityID, req.SrcEntityID, req.EvidenceFactID, actor)
	if err != nil {
		writeFactActionError(w, err)
		return
	}
	writeFactActionOK(w, mergeID)
}

// entitySplitRequest is POST /v1/entity/split's body: `kahya entity split
// <merge_ledger_id>`.
type entitySplitRequest struct {
	MergeLedgerID int64  `json:"merge_ledger_id"`
	Actor         string `json:"actor"`
}

func (s *Server) handleEntitySplit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.factEngine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "fact engine not available")
		return
	}
	var req entitySplitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MergeLedgerID == 0 {
		writeJSONError(w, http.StatusBadRequest, "merge_ledger_id is required")
		return
	}
	actor := req.Actor
	if actor == "" {
		actor = "user"
	}
	traceID := traceIDFromContext(r)
	splitID, err := s.factEngine.SplitEntities(r.Context(), traceID, req.MergeLedgerID, actor)
	if err != nil {
		writeFactActionError(w, err)
		return
	}
	writeFactActionOK(w, splitID)
}

func writeFactActionOK(w http.ResponseWriter, id int64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(factActionResponse{OK: true, ID: id})
}

func writeFactActionError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(factActionResponse{OK: false, Error: err.Error()})
}

// EntityReader is the read side of the entity layer the GET /v1/entity/*
// routes need (G13 MEM-2) - *factengine.Engine satisfies it.
type EntityReader interface {
	ListEntities(ctx context.Context, prefix string, provisionalOnly bool, limit int) ([]factengine.EntitySummary, error)
	ShowEntity(ctx context.Context, id int64) (factengine.EntityDetail, error)
}

var _ EntityReader = (*factengine.Engine)(nil)

// SetEntityReader wires GET /v1/entity/list and GET /v1/entity/show/{id}.
// Call before Prepare(); both answer 503 until it is set.
func (s *Server) SetEntityReader(r EntityReader) {
	s.entityReader = r
}

type entityListResponse struct {
	Entities []factengine.EntitySummary `json:"entities"`
}

// handleEntityList answers GET /v1/entity/list?q=<name prefix>&
// provisional=1&limit=<n>: active entities whose folded alias starts with
// the folded q (factengine.ListEntities; limit default 50, at most 200).
func (s *Server) handleEntityList(w http.ResponseWriter, r *http.Request) {
	if s.entityReader == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "entity reads not available")
		return
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	provisional := q.Get("provisional") == "1" || q.Get("provisional") == "true"
	ents, err := s.entityReader.ListEntities(r.Context(), q.Get("q"), provisional, limit)
	if err != nil {
		s.log.With(traceIDFromContext(r)).Error("entity_list_failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ents == nil {
		ents = []factengine.EntitySummary{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(entityListResponse{Entities: ents})
}

// handleEntityShow answers GET /v1/entity/show/{id}: the entity, its
// aliases and the active facts touching it (quarantined ones labelled -
// a local surface, never Telegram or a prompt).
func (s *Server) handleEntityShow(w http.ResponseWriter, r *http.Request) {
	if s.entityReader == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "entity reads not available")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeJSONError(w, http.StatusBadRequest, "entity id must be a positive integer")
		return
	}
	d, err := s.entityReader.ShowEntity(r.Context(), id)
	if errors.Is(err, factengine.ErrEntityNotFound) {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.log.With(traceIDFromContext(r)).Error("entity_show_failed", "entity_id", id, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d.Facts == nil {
		d.Facts = []factengine.EntityFact{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(d)
}
