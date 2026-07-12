// collect_gh.go implements the W5-01 GitHub collector: deterministic Go
// code (never model-written shell - the Docker-shell rule, HANDOFF §5
// safety #6, targets model-authored commands, not this fixed, kahyad-
// authored invocation), running ONLY the two fixed read-only `gh`
// subcommands the task spec names ("gh pr list", "gh run list --limit
// 10"), per configured repo, JSON-parsed into typed structs. `gh` itself
// egresses over HTTPS to api.github.com - ExecGHRunner points HTTPS_PROXY/
// https_proxy at the W3-05 egress proxy (policy.yaml already allowlists
// api.github.com) so this traffic passes through the same gate every other
// off-box byte does.
package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

// ghItemsPerRepo is the task spec's own fixed cap ("gh run list --limit
// 10") - applied to BOTH `gh pr list` and `gh run list` for the same
// "bounded, never unbounded free text into a prompt" reason every
// collector in this package follows.
const ghItemsPerRepo = 10

// ghTitleMaxLen/ghConclusionMaxLen are this collector's own length caps
// (charclass-constrained via capText below) - a PR title or workflow name
// is a short label, never a paragraph.
const (
	ghTitleMaxLen      = 200
	ghConclusionMaxLen = 40
)

// GHPullRequest is one collected, already length/charclass-capped GitHub
// pull request signal.
type GHPullRequest struct {
	Repo   string
	Number int
	Title  string
}

// GHRun is one collected, already length/charclass-capped GitHub Actions
// run signal.
type GHRun struct {
	Repo       string
	Workflow   string
	Conclusion string
}

// GHRunner is the narrow "invoke `gh` with these fixed args, return its
// raw stdout" seam. ExecGHRunner is the production implementation; tests
// inject a fake that never shells out at all.
type GHRunner interface {
	Run(ctx context.Context, args []string) ([]byte, error)
}

// ExecGHRunner is the production GHRunner: os/exec against the real `gh`
// binary, with HTTPS_PROXY/https_proxy set to the W3-05 egress proxy
// address (e.g. "http://127.0.0.1:3128") so `gh`'s own HTTPS traffic to
// api.github.com passes through kahyad's egress gate like every other
// off-box byte.
type ExecGHRunner struct {
	// GHPath defaults to "gh" (resolved via PATH) when empty.
	GHPath string
	// HTTPSProxy is the W3-05 egress proxy's own "http://127.0.0.1:<port>"
	// address. Empty disables proxying (never used in production - main.go
	// always sets this from cfg.EgressPort).
	HTTPSProxy string
}

var _ GHRunner = ExecGHRunner{}

// Run implements GHRunner.
func (r ExecGHRunner) Run(ctx context.Context, args []string) ([]byte, error) {
	ghPath := r.GHPath
	if ghPath == "" {
		ghPath = "gh"
	}
	cmd := exec.CommandContext(ctx, ghPath, args...)
	env := os.Environ()
	if r.HTTPSProxy != "" {
		env = append(env, "HTTPS_PROXY="+r.HTTPSProxy, "https_proxy="+r.HTTPSProxy)
	}
	cmd.Env = env
	return cmd.Output()
}

// GHCollector collects PRs/runs for every configured repo via Runner.
type GHCollector struct {
	Runner GHRunner
	Repos  []string
}

type ghPullRequestJSON struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

type ghRunJSON struct {
	WorkflowName string `json:"workflowName"`
	Conclusion   string `json:"conclusion"`
}

// Collect runs `gh pr list`/`gh run list` for every configured repo. A
// nil Runner or empty Repos list is a documented no-op (nil, nil, nil) -
// the gh section of a briefing simply has zero items until an operator
// configures repos, never an error. A single repo's (or single
// subcommand's) failure is NON-FATAL: it is skipped, never aborting the
// whole briefing (kahyad's own collectors must be at least as tolerant of
// a transient `gh`/network hiccup as the rest of this task's design is of
// a missing Calendar TCC grant).
func (c GHCollector) Collect(ctx context.Context) ([]GHPullRequest, []GHRun, error) {
	if c.Runner == nil || len(c.Repos) == 0 {
		return nil, nil, nil
	}

	var prs []GHPullRequest
	var runs []GHRun
	limit := strconv.Itoa(ghItemsPerRepo)

	for _, repo := range c.Repos {
		if prOut, err := c.Runner.Run(ctx, []string{
			"pr", "list", "--repo", repo, "--state", "open", "--limit", limit, "--json", "number,title",
		}); err == nil {
			var items []ghPullRequestJSON
			if json.Unmarshal(prOut, &items) == nil {
				for _, it := range items {
					prs = append(prs, GHPullRequest{Repo: repo, Number: it.Number, Title: capText(it.Title, ghTitleMaxLen)})
				}
			}
		}

		if runOut, err := c.Runner.Run(ctx, []string{
			"run", "list", "--repo", repo, "--limit", limit, "--json", "workflowName,conclusion",
		}); err == nil {
			var items []ghRunJSON
			if json.Unmarshal(runOut, &items) == nil {
				for _, it := range items {
					runs = append(runs, GHRun{
						Repo: repo, Workflow: capText(it.WorkflowName, ghTitleMaxLen),
						Conclusion: capText(it.Conclusion, ghConclusionMaxLen),
					})
				}
			}
		}
	}
	return prs, runs, nil
}

// ghPullRequestItems adapts a []GHPullRequest into the gate's uniform
// []CollectedItem shape (gate.go) - no Path (gh items carry no filesystem
// path), Section="gh_pr".
func ghPullRequestItems(prs []GHPullRequest) []CollectedItem {
	items := make([]CollectedItem, len(prs))
	for i, pr := range prs {
		items[i] = CollectedItem{Section: "gh_pr", Text: fmt.Sprintf("%s#%d: %s", pr.Repo, pr.Number, pr.Title)}
	}
	return items
}

// ghRunItems adapts a []GHRun into []CollectedItem, Section="gh_run".
func ghRunItems(runs []GHRun) []CollectedItem {
	items := make([]CollectedItem, len(runs))
	for i, r := range runs {
		items[i] = CollectedItem{Section: "gh_run", Text: fmt.Sprintf("%s: %s (%s)", r.Repo, r.Workflow, r.Conclusion)}
	}
	return items
}
