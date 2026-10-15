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
// patterns in Go's ServeMux. G13 MEM-5 phase 1 adds the human review side:
// bulk POST /v1/fact/confirm (fact_ids | episode_id | all_quarantined_since),
// POST /v1/fact/deny (`kahya fact deny <id>...`) and GET /v1/fact/list
// (`kahya fact list`). All are control routes (G03): a worker can never lift
// a quarantine or lower a fact's confidence (HANDOFF §5 memory #1).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// G13 MEM-5 phase 1.
	DenyFact(ctx context.Context, traceID string, factID int64, sessionID string) error
	ListFacts(ctx context.Context, q factengine.FactListQuery) ([]factengine.FactListItem, bool, error)
	FactIDsByEpisode(ctx context.Context, episodeID int64) ([]int64, error)
	QuarantinedFactIDsSince(ctx context.Context, since time.Time) ([]int64, error)
}

var _ FactEngineRunner = (*factengine.Engine)(nil)

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

// factConfirmRequest is POST /v1/fact/confirm's body - exactly one
// selector: {"fact_id": N} (W5-04, answered {"ok":true,"id":N} as before),
// {"fact_ids": [...]}, {"episode_id": E} (every active fact that episode
// evidences) or {"all_quarantined_since": "<RFC3339>"} (every quarantined
// fact created since then; all_quarantined:true without the bound is
// refused - confirming everything by accident is the failure mode the bound
// exists for). A bulk answer is {"ok":true,"confirmed":[...],"failed":{...}}.
type factConfirmRequest struct {
	FactID              int64   `json:"fact_id"`
	FactIDs             []int64 `json:"fact_ids"`
	EpisodeID           int64   `json:"episode_id"`
	AllQuarantined      bool    `json:"all_quarantined"`
	AllQuarantinedSince string  `json:"all_quarantined_since"`
}

// factDenyRequest is POST /v1/fact/deny's body: {"fact_id": N} or
// {"fact_ids": [...]}, plus the session the denial is attributed to ("" =
// "cli:<trace_id>", one evidence session per CLI call: the same id twice in
// one call is one denial - HANDOFF §5 memory #3).
type factDenyRequest struct {
	FactID    int64   `json:"fact_id"`
	FactIDs   []int64 `json:"fact_ids"`
	SessionID string  `json:"session_id"`
}

// factBulkResponse is the bulk confirm/deny answer: the ids acted on (in
// request order, each once) and the per-id failures.
type factBulkResponse struct {
	OK        bool              `json:"ok"`
	Confirmed []int64           `json:"confirmed,omitempty"`
	Denied    []int64           `json:"denied,omitempty"`
	Failed    map[string]string `json:"failed"`
	TraceID   string            `json:"trace_id"`
}

// handleFactConfirm implements `kahya fact confirm <id>... | --episode <id>
// | --all-quarantined --since <RFC3339>` (lifts the quarantine half of
// factengine.InjectionEligible - see that predicate's own doc comment).
// Every confirmation is its own fact_confirmed ledger row; at most
// factengine.MaxFactBatch facts per call.
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	selectors := 0
	for _, set := range []bool{req.FactID != 0, req.FactIDs != nil, req.EpisodeID != 0, req.AllQuarantined || req.AllQuarantinedSince != ""} {
		if set {
			selectors++
		}
	}
	if selectors != 1 {
		writeJSONError(w, http.StatusBadRequest, "give exactly one of fact_id, fact_ids, episode_id, all_quarantined_since")
		return
	}
	traceID := traceIDFromContext(r)
	ctx := r.Context()
	if req.FactID != 0 {
		if err := s.factEngine.ConfirmFact(ctx, traceID, req.FactID); err != nil {
			writeFactActionError(w, err)
			return
		}
		writeFactActionOK(w, req.FactID)
		return
	}
	var ids []int64
	var err error
	switch {
	case req.FactIDs != nil:
		ids, err = bulkFactIDs(req.FactIDs)
	case req.EpisodeID != 0:
		if req.EpisodeID < 0 {
			writeJSONError(w, http.StatusBadRequest, "episode_id must be a positive integer")
			return
		}
		ids, err = s.factEngine.FactIDsByEpisode(ctx, req.EpisodeID)
	default:
		if req.AllQuarantinedSince == "" {
			writeJSONError(w, http.StatusBadRequest, "all_quarantined_since (RFC3339) is required")
			return
		}
		since, perr := time.Parse(time.RFC3339, req.AllQuarantinedSince)
		if perr != nil {
			writeJSONError(w, http.StatusBadRequest, "all_quarantined_since must be RFC3339")
			return
		}
		ids, err = s.factEngine.QuarantinedFactIDsSince(ctx, since)
	}
	if err != nil {
		writeFactSelectionError(w, err)
		return
	}
	resp := factBulkResponse{OK: true, Confirmed: []int64{}, Failed: map[string]string{}, TraceID: traceID}
	for _, id := range ids {
		if err := s.factEngine.ConfirmFact(ctx, traceID, id); err != nil {
			resp.Failed[strconv.FormatInt(id, 10)] = err.Error()
			continue
		}
		resp.Confirmed = append(resp.Confirmed, id)
	}
	writeFactBulk(w, resp)
}

// handleFactDeny implements `kahya fact deny <id>...`: each fact gets one
// fixed -2.94 user-denial evidence row (factengine.DenyFact - it is not
// closed, unlike a retraction), at most factengine.MaxFactBatch per call.
func (s *Server) handleFactDeny(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.factEngine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "fact engine not available")
		return
	}
	var req factDenyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if (req.FactID != 0) == (req.FactIDs != nil) {
		writeJSONError(w, http.StatusBadRequest, "give exactly one of fact_id, fact_ids")
		return
	}
	traceID := traceIDFromContext(r)
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = "cli:" + traceID
	}
	ctx := r.Context()
	if req.FactID != 0 {
		if err := s.factEngine.DenyFact(ctx, traceID, req.FactID, sessionID); err != nil {
			writeFactActionError(w, err)
			return
		}
		writeFactActionOK(w, req.FactID)
		return
	}
	ids, err := bulkFactIDs(req.FactIDs)
	if err != nil {
		writeFactSelectionError(w, err)
		return
	}
	resp := factBulkResponse{OK: true, Denied: []int64{}, Failed: map[string]string{}, TraceID: traceID}
	for _, id := range ids {
		if err := s.factEngine.DenyFact(ctx, traceID, id, sessionID); err != nil {
			resp.Failed[strconv.FormatInt(id, 10)] = err.Error()
			continue
		}
		resp.Denied = append(resp.Denied, id)
	}
	writeFactBulk(w, resp)
}

// errBadFactID is a non-positive id in a fact_ids list.
var errBadFactID = errors.New("fact_ids must be positive integers")

// bulkFactIDs validates a fact_ids list: non-empty, positive ids, each kept
// once in request order, at most factengine.MaxFactBatch.
func bulkFactIDs(raw []int64) ([]int64, error) {
	if len(raw) == 0 {
		return nil, errors.New("fact_ids must not be empty")
	}
	seen := make(map[int64]bool, len(raw))
	ids := make([]int64, 0, len(raw))
	for _, id := range raw {
		if id < 1 {
			return nil, errBadFactID
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) > factengine.MaxFactBatch {
		return nil, factengine.ErrFactBatchTooLarge
	}
	return ids, nil
}

// writeFactSelectionError answers a selection that could not be resolved:
// 400 for a malformed or oversized selection, 500 otherwise.
func writeFactSelectionError(w http.ResponseWriter, err error) {
	if errors.Is(err, factengine.ErrFactBatchTooLarge) {
		writeJSONError(w, http.StatusBadRequest, factengine.ErrFactBatchTooLarge.Error())
		return
	}
	if errors.Is(err, errBadFactID) || strings.HasPrefix(err.Error(), "fact_ids") {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONError(w, http.StatusInternalServerError, err.Error())
}

func writeFactBulk(w http.ResponseWriter, resp factBulkResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// factListResponse is GET /v1/fact/list's answer; truncated reports that
// more matching facts exist beyond the limit.
type factListResponse struct {
	Facts     []factengine.FactListItem `json:"facts"`
	Truncated bool                      `json:"truncated"`
	TraceID   string                    `json:"trace_id"`
}

// handleFactList answers GET /v1/fact/list?quarantined=1&since=<RFC3339>&
// entity=<id|ad>&limit=<n> (`kahya fact list`): active facts newest first -
// only the ones still waiting for a human confirmation with quarantined=1 -
// each with its first evidence note. A LOCAL surface (control route): secret
// -lane facts are listed too.
func (s *Server) handleFactList(w http.ResponseWriter, r *http.Request) {
	if s.factEngine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "fact engine not available")
		return
	}
	v := r.URL.Query()
	q := factengine.FactListQuery{QuarantinedOnly: v.Get("quarantined") == "1" || v.Get("quarantined") == "true"}
	if raw := strings.TrimSpace(v.Get("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "since must be RFC3339")
			return
		}
		q.CreatedSince = t
	}
	if e := strings.TrimSpace(v.Get("entity")); e != "" {
		if id, err := strconv.ParseInt(e, 10, 64); err == nil {
			if id < 1 {
				writeJSONError(w, http.StatusBadRequest, "entity id must be a positive integer")
				return
			}
			q.EntityID = id
		} else {
			q.EntityName = e
		}
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > factengine.MaxFactBatch {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("limit must be 1..%d", factengine.MaxFactBatch))
			return
		}
		q.Limit = n
	}
	traceID := traceIDFromContext(r)
	facts, truncated, err := s.factEngine.ListFacts(r.Context(), q)
	if err != nil {
		s.log.With(traceID).Error("fact_list_failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if facts == nil {
		facts = []factengine.FactListItem{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(factListResponse{Facts: facts, Truncated: truncated, TraceID: traceID})
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
