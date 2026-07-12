package briefing

import (
	"context"
	"testing"
)

func TestGHCollectorParsesPRsAndRuns(t *testing.T) {
	runner := &fakeGHRunner{
		PRJSON:  []byte(`[{"number":12,"title":"bump deps"},{"number":13,"title":"fix flake"}]`),
		RunJSON: []byte(`[{"workflowName":"CI","conclusion":"success"}]`),
	}
	c := GHCollector{Runner: runner, Repos: []string{"kahya/gold-token"}}

	prs, runs, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(prs) != 2 || prs[0].Repo != "kahya/gold-token" || prs[0].Number != 12 || prs[0].Title != "bump deps" {
		t.Fatalf("prs = %+v, unexpected", prs)
	}
	if len(runs) != 1 || runs[0].Workflow != "CI" || runs[0].Conclusion != "success" {
		t.Fatalf("runs = %+v, unexpected", runs)
	}

	// Fixed read-only args only (task spec: "gh pr list", "gh run list
	// --limit 10").
	if len(runner.Calls) != 2 {
		t.Fatalf("gh invocations = %d, want 2 (one pr list, one run list)", len(runner.Calls))
	}
	if runner.Calls[0][0] != "pr" || runner.Calls[0][1] != "list" {
		t.Errorf("first call = %v, want to start with [pr list]", runner.Calls[0])
	}
	if runner.Calls[1][0] != "run" || runner.Calls[1][1] != "list" {
		t.Errorf("second call = %v, want to start with [run list]", runner.Calls[1])
	}
}

func TestGHCollectorNoRunnerOrNoReposIsNoop(t *testing.T) {
	prs, runs, err := (GHCollector{}).Collect(context.Background())
	if err != nil || prs != nil || runs != nil {
		t.Fatalf("Collect(no runner/repos) = %v, %v, %v, want nil, nil, nil", prs, runs, err)
	}
}

func TestGHCollectorTitleIsLengthAndCharclassCapped(t *testing.T) {
	longTitle := ""
	for i := 0; i < 50; i++ {
		longTitle += "very long title with\nnewline "
	}
	runner := &fakeGHRunner{PRJSON: []byte(`[{"number":1,"title":"` + jsonEscape(longTitle) + `"}]`)}
	c := GHCollector{Runner: runner, Repos: []string{"kahya/x"}}

	prs, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("prs = %+v, want 1 entry", prs)
	}
	if n := len([]rune(prs[0].Title)); n > ghTitleMaxLen {
		t.Errorf("Title rune length = %d, want <= %d", n, ghTitleMaxLen)
	}
	for _, r := range prs[0].Title {
		if r == '\n' {
			t.Fatalf("Title %q still contains a raw newline", prs[0].Title)
		}
	}
}

// jsonEscape is a tiny test-only helper for embedding a raw string
// (possibly containing newlines) inside a hand-written JSON fixture.
func jsonEscape(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r == '\n' {
			out = append(out, '\\', 'n')
			continue
		}
		out = append(out, string(r)...)
	}
	return string(out)
}
