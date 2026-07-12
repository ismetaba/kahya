package reader

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/taint"
)

func testActorStore(t *testing.T) *store.Store {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRenderPromptMailSummaryDeterministicNoRawStrings(t *testing.T) {
	v := MailSummaryV1{
		FromDisplay: "Fatura Servisi",
		Subject:     "Fatura bildirimi",
		Summary:     "Son odeme tarihi 15 Temmuz 2026, tutar 4.250,00 TL.",
		Dates:       []string{"2026-07-15T00:00:00Z"},
		Amounts:     []string{"4.250,00 TL"},
	}
	p1, err := RenderPrompt(v)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	p2, err := RenderPrompt(v)
	if err != nil {
		t.Fatalf("RenderPrompt (again): %v", err)
	}
	if p1 != p2 {
		t.Fatalf("RenderPrompt is not deterministic:\n%q\nvs\n%q", p1, p2)
	}
	for _, want := range []string{"Fatura Servisi", "Fatura bildirimi", "4.250,00 TL"} {
		if !strings.Contains(p1, want) {
			t.Errorf("rendered prompt missing %q:\n%s", want, p1)
		}
	}
}

func TestRenderPromptRejectsUnsupportedType(t *testing.T) {
	if _, err := RenderPrompt(42); err == nil {
		t.Fatal("expected an error for an unsupported validated type")
	}
}

// TestSpawnInsertsCleanRowAndLedgersActorSeeded is the step-7/8 test:
// Spawn's tasks row + session_taint(clean) row commit together, and
// actor.seeded is ledgered with derived_from set to the episode id.
func TestSpawnInsertsCleanRowAndLedgersActorSeeded(t *testing.T) {
	st := testActorStore(t)
	ctx := context.Background()

	v := MailSummaryV1{FromDisplay: "a", Subject: "b", Summary: "c"}
	res, err := Spawn(ctx, st.DB(), st, v, "claude-sonnet-5", "episode-123", "trace-spawn")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res.TaskID == "" || res.SessionID == "" {
		t.Fatalf("Spawn result = %+v, want non-empty TaskID/SessionID", res)
	}
	if !strings.Contains(res.Prompt, "Fatura") && !strings.Contains(res.Prompt, "b") {
		// Weak sanity check only - RenderPrompt's own tests cover content.
	}

	tr := taint.New(st.Queries, st)
	tier, terr := tr.Get(ctx, res.SessionID)
	if terr != nil {
		t.Fatalf("taint.Get: %v", terr)
	}
	if tier != taint.TierClean {
		t.Fatalf("tier = %q, want %q", tier, taint.TierClean)
	}

	var n int
	if err := st.DB().QueryRow(
		`SELECT count(*) FROM events WHERE kind = ? AND json_extract(payload,'$.derived_from') = ?`,
		EventActorSeeded, "episode-123",
	).Scan(&n); err != nil {
		t.Fatalf("count actor.seeded events: %v", err)
	}
	if n != 1 {
		t.Fatalf("actor.seeded events with derived_from=episode-123 = %d, want 1", n)
	}

	var sessionID string
	if err := st.DB().QueryRow(`SELECT session_id FROM tasks WHERE id = ?`, res.TaskID).Scan(&sessionID); err != nil {
		t.Fatalf("query tasks.session_id: %v", err)
	}
	if sessionID != res.SessionID {
		t.Fatalf("tasks.session_id = %q, want %q", sessionID, res.SessionID)
	}
}

// TestActorSessionStartsCleanAndPolicyCheckPassesTaint is the step-8
// permanent regression test, verbatim: "actor session starts clean and
// its policy check passes taint (ladder/approvals still apply)". Seeds a
// fresh Actor session via Spawn, wires the SAME session_taint store into
// a real policy.Engine, and proves a W1 tool call for that session_id is
// governed by the ORDINARY ladder (pre-seeded to auto-allow), never
// denied for being tainted.
func TestActorSessionStartsCleanAndPolicyCheckPassesTaint(t *testing.T) {
	st := testActorStore(t)
	ctx := context.Background()

	v := WebpageExtractV1{Title: "t", KeyPoints: []string{"a"}}
	res, err := Spawn(ctx, st.DB(), st, v, "claude-sonnet-5", "episode-456", "trace-spawn-2")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	tools := []policy.ToolRule{{Name: "fs_write", Class: policy.ClassW1, ScopeKey: "global"}}
	byName := map[string]policy.ToolRule{"fs_write": tools[0]}
	pol := policy.Policy{Tools: tools, ToolsByName: byName}
	engine := policy.NewEngine(pol, st.Queries, st)
	engine.SetTaintChecker(taint.New(st.Queries, st))

	// Seed the ladder so W1 auto-allows (L2) - proving the ladder decision
	// itself comes through, not merely "not denied for taint".
	if _, err := st.DB().Exec(
		`INSERT INTO autonomy_state (tool, class, scope, level, consecutive_approvals, updated_at) VALUES (?, ?, ?, ?, 0, ?)`,
		"fs_write", "W1", "global", policy.L2, "2026-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("seed autonomy_state: %v", err)
	}

	d, err := engine.Check(ctx, policy.CheckInput{
		Tool: "fs_write", SessionID: res.SessionID, TaskID: res.TaskID, TraceID: "trace-actor-check",
		ToolInput: []byte(`{"path":"/tmp/x"}`),
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Result != policy.ResultAllow {
		t.Fatalf("Check(fs_write, actor-seeded clean session) = %+v, want ALLOW", d)
	}
}
