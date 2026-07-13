// gate.go implements W78-01's reusable pre-change gate (§5-Memory-#5:
// "her konsolidasyon/gömülü/füzyon değişikliğinden ÖNCE kapı"). A change is
// allowed only when a GREEN eval.retrieval.result event exists that is
// recent enough AND matches the current index state (dataset_sha256,
// model_ver, fusion_sha256). Every refusal carries the byte-exact Turkish
// message GateRefusalReason and is fail-closed: any error reading the ledger
// REFUSES, never allows.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// GateRefusalReason is the byte-exact Turkish refusal every gated point
// surfaces/ledgers when the retrieval eval gate is not green (task spec:
// verbatim). Never reword, re-punctuate, or ASCII-fold it.
const GateRefusalReason = "retrieval eval kapısı yeşil değil — önce 'kahya eval retrieval' çalıştır"

// GateState is the current index state a green result must match.
type GateState struct {
	DatasetSHA256 string
	ModelVer      string
	FusionSHA256  string
}

// GateResult is EvalGate.Check's verdict. Reason is empty when Allowed;
// otherwise it is GateRefusalReason (byte-exact Turkish, for the user/
// ledger). Detail is an English, log-only explanation of WHY it refused
// (never surfaced to the user).
type GateResult struct {
	Allowed bool
	Reason  string
	Detail  string
}

// EvalGate is the pre-change gate. It reads eval.retrieval.result events via
// Reader and (with Now, overridable in tests) enforces the freshness window.
type EvalGate struct {
	Reader EventReader
	Now    func() time.Time
}

func (g EvalGate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// retrievalResultPayload is the eval.retrieval.result payload shape the gate
// reads back (a subset of what RetrievalRunner.Run writes).
type retrievalResultPayload struct {
	Precision     float64 `json:"precision"`
	DatasetSHA256 string  `json:"dataset_sha256"`
	ModelVer      string  `json:"model_ver"`
	FusionSHA256  string  `json:"fusion_sha256"`
}

// Check reports whether a green eval.retrieval.result exists that is no
// older than maxAge and whose dataset_sha256/model_ver/fusion_sha256 all
// match want. "Green" = precision >= MinPrecision. Any matching green event
// within the window allows; the newest is not required.
//
// Fail-closed: a nil reader or a reader error REFUSES (Allowed=false) with
// GateRefusalReason - a gate that cannot read its evidence must deny, never
// permit.
func (g EvalGate) Check(ctx context.Context, maxAge time.Duration, want GateState) GateResult {
	if g.Reader == nil {
		return refuse("no event reader wired")
	}
	rows, err := g.Reader.ListEventsByKind(ctx, EventRetrievalResult)
	if err != nil {
		return refuse("list " + EventRetrievalResult + " events: " + err.Error())
	}
	cutoff := g.now().Add(-maxAge)
	for _, row := range rows {
		createdAt, perr := time.Parse(time.RFC3339, row.CreatedAt)
		if perr != nil {
			continue // a row with an unparseable timestamp cannot prove freshness
		}
		if createdAt.Before(cutoff) {
			continue // stale
		}
		var p retrievalResultPayload
		if json.Unmarshal([]byte(row.Payload), &p) != nil {
			continue
		}
		if p.Precision < MinPrecision {
			continue // red
		}
		if p.DatasetSHA256 != want.DatasetSHA256 || p.ModelVer != want.ModelVer || p.FusionSHA256 != want.FusionSHA256 {
			continue // does not match the current index state
		}
		return GateResult{Allowed: true}
	}
	return refuse(fmt.Sprintf("no green %s event within %s matching dataset/model/fusion", EventRetrievalResult, maxAge))
}

// CheckRetrievalGate is the primitive-typed adaptation
// kahyad/internal/consolidation and kahyad/internal/embed consume (each
// defines its own narrow gate interface with this exact signature, so
// neither has to import this package). It forwards to Check.
func (g EvalGate) CheckRetrievalGate(ctx context.Context, maxAge time.Duration, datasetSHA, modelVer, fusionSHA string) (allowed bool, reason, detail string) {
	res := g.Check(ctx, maxAge, GateState{DatasetSHA256: datasetSHA, ModelVer: modelVer, FusionSHA256: fusionSHA})
	return res.Allowed, res.Reason, res.Detail
}

// CheckFusionActivation is the search.FusionGate adaptation (gate point c):
// it asks whether a green result covers a candidate fusion_sha256 within the
// freshness window, matching the CURRENT dataset_sha256/model_ver. Because
// the fusion config is a boot-time literal today, this is only reachable
// through search.Searcher.ActivateFusionConfig, which has no live caller yet
// - it exists so a future fusion change is gated by construction. maxAge and
// the current dataset/model state are captured on the gate value via its
// fields below.
func (g FusionActivationGate) CheckFusionActivation(fusionSHA string) (bool, string) {
	res := g.Gate.Check(g.ctx(), g.MaxAge, GateState{DatasetSHA256: g.DatasetSHA256, ModelVer: g.ModelVer, FusionSHA256: fusionSHA})
	return res.Allowed, res.Reason
}

// FusionActivationGate adapts an EvalGate to search.FusionGate: it binds the
// current dataset_sha256/model_ver and freshness window so a fusion change
// is scored against a green result for THIS index state carrying the
// candidate fusion_sha256.
type FusionActivationGate struct {
	Gate          EvalGate
	MaxAge        time.Duration
	DatasetSHA256 string
	ModelVer      string
	Ctx           context.Context
}

func (g FusionActivationGate) ctx() context.Context {
	if g.Ctx != nil {
		return g.Ctx
	}
	return context.Background()
}

// ReEmbedGateAdapter adapts an EvalGate to kahyad/internal/embed's re_embed
// activation gate (gate point b). It (re)loads the dataset on EACH check so
// dataset_sha256 always reflects the file on disk at activation time, and
// matches against the captured active fusion_sha256. A missing/unreadable
// dataset is fail-closed: the activation is refused (a model_ver switch
// cannot proceed without the real dataset a green run was scored against).
type ReEmbedGateAdapter struct {
	Gate         EvalGate
	DatasetPath  string
	FusionSHA256 string
	MaxAge       time.Duration
}

// AllowReEmbedActivation satisfies kahyad/internal/embed's reEmbedGate.
func (a ReEmbedGateAdapter) AllowReEmbedActivation(ctx context.Context, modelVer string) (allowed bool, reason, detail string) {
	ds, err := LoadRetrievalDataset(a.DatasetPath)
	if err != nil {
		return false, GateRefusalReason, "load retrieval dataset: " + err.Error()
	}
	return a.Check(ctx, ds.SHA256, modelVer)
}

// Check is AllowReEmbedActivation's inner gate call, split out so tests can
// drive it with a known dataset_sha256 without a dataset file on disk.
func (a ReEmbedGateAdapter) Check(ctx context.Context, datasetSHA, modelVer string) (allowed bool, reason, detail string) {
	return a.Gate.CheckRetrievalGate(ctx, a.MaxAge, datasetSHA, modelVer, a.FusionSHA256)
}

// refuse builds a fail-closed GateResult with the byte-exact Turkish reason
// and an English log-only detail.
func refuse(detail string) GateResult {
	return GateResult{Allowed: false, Reason: GateRefusalReason, Detail: detail}
}
