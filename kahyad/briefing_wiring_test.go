package main

import (
	"go/ast"
	"testing"
)

// compositeLitFields returns, for every composite literal of type
// pkg.typeName in main.go, the source text of each keyed field's value.
func compositeLitFields(t *testing.T, pkg, typeName string) []map[string]string {
	t.Helper()
	fset, f, src := mainSource(t)
	var out []map[string]string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != typeName {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != pkg {
			return true
		}
		fields := map[string]string{}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			fields[key.Name] = string(src[fset.Position(kv.Value.Pos()).Offset:fset.Position(kv.Value.End()).Offset])
		}
		out = append(out, fields)
		return true
	})
	return out
}

// onlyCompositeLit fails unless main.go builds exactly one pkg.typeName
// literal, and returns its keyed fields.
func onlyCompositeLit(t *testing.T, pkg, typeName string) map[string]string {
	t.Helper()
	lits := compositeLitFields(t, pkg, typeName)
	if len(lits) != 1 {
		t.Fatalf("%s.%s literals in main.go = %d, want exactly 1", pkg, typeName, len(lits))
	}
	return lits[0]
}

// TestBriefingClassifierWarmupWiring (FIX-P-4): the morning briefing warms
// the SAME Qwen supervisor its classifier's adapter calls (never a second
// one), and that supervisor's warm per-call budget comes from
// cfg.qwen_classify_budget_ms.
func TestBriefingClassifierWarmupWiring(t *testing.T) {
	orch := onlyCompositeLit(t, "briefing", "Orchestrator")
	if orch["Warmer"] != "qwenSup" {
		t.Errorf("briefing.Orchestrator.Warmer = %q, want qwenSup", orch["Warmer"])
	}
	if orch["Classifier"] != "secretLaneClassifier" {
		t.Errorf("briefing.Orchestrator.Classifier = %q, want secretLaneClassifier", orch["Classifier"])
	}
	onlyCallSite(t, "mlx", "NewQwenClassifierAdapter", "qwenSup, cfg.QwenModelName")

	sup := onlyCompositeLit(t, "mlx", "Config")
	if want := "time.Duration(cfg.QwenClassifyBudgetMS) * time.Millisecond"; sup["ClassifyWarmBudget"] != want {
		t.Errorf("mlx.Config.ClassifyWarmBudget = %q, want %q", sup["ClassifyWarmBudget"], want)
	}
}

// TestBriefingFileCollectorCapWiring (FIX-P-23): the briefing's changed-
// file collector is capped by cfg.briefing_max_file_items.
func TestBriefingFileCollectorCapWiring(t *testing.T) {
	files := onlyCompositeLit(t, "briefing", "FileGlobCollector")
	if files["MaxItems"] != "cfg.BriefingMaxFileItems" {
		t.Errorf("briefing.FileGlobCollector.MaxItems = %q, want cfg.BriefingMaxFileItems", files["MaxItems"])
	}
	if files["Globs"] != "cfg.BriefingFileGlobs" {
		t.Errorf("briefing.FileGlobCollector.Globs = %q, want cfg.BriefingFileGlobs", files["Globs"])
	}
}

// TestBriefingLocationWiring (FIX-P-14): the briefing's once-per-day gate
// computes its day in cfg.Location() - kahyad's single calendar zone
// source (G00 C-22) - and uses the ledger-backed dedupe.
func TestBriefingLocationWiring(t *testing.T) {
	orch := onlyCompositeLit(t, "briefing", "Orchestrator")
	if orch["Location"] != "cfg.Location()" {
		t.Errorf("briefing.Orchestrator.Location = %q, want cfg.Location()", orch["Location"])
	}
	if orch["Dedupe"] != "briefing.StoreDedupeChecker{Store: st.Queries}" {
		t.Errorf("briefing.Orchestrator.Dedupe = %q, want the store-backed checker", orch["Dedupe"])
	}
}
