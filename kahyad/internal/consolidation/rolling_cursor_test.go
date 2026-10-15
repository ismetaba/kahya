package consolidation

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"kahya/kahyad/internal/mlx"
)

// FIX-B-5 phase 3 (review): the rolling cursor keeps the order a night left
// its unsent files in. Night N+1 continues where night N stopped and the
// files night N did send join the back of the queue, so a lane larger than
// one night's max_batches_per_lane rotates through every file in
// ceil(files / batches per night) nights - none is starved, a file the user
// edits every day included. Re-sorting the carried files by (dir, name)
// every night made the sent set alternate between two head groups and the
// tail was never reached.

// sentPaths flattens one night's recorded session calls into the paths
// sent, in call order.
func sentPaths(calls []map[string]string) []string {
	var out []string
	for _, call := range calls {
		out = append(out, sortedKeys(call)...)
	}
	return out
}

// assertEveryWindowSendsAll fails unless every run of window consecutive
// nights sent every file of all at least once.
func assertEveryWindowSendsAll(t *testing.T, nights [][]string, all []string, window int) {
	t.Helper()
	for end := window; end <= len(nights); end++ {
		seen := map[string]bool{}
		for _, n := range nights[end-window : end] {
			for _, f := range n {
				seen[f] = true
			}
		}
		var missing []string
		for _, f := range all {
			if !seen[f] {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			t.Fatalf("nights %d-%d never sent %q (sent per night: %q)", end-window+1, end, missing, nights)
		}
	}
}

// seedEqualCloudCorpus gives the repo six cloud notes of one size (note.md
// plus topic00..04/note.md) and returns the batch budget that fits exactly
// one of them, with room for the lines the nights append.
func seedEqualCloudCorpus(t *testing.T, repo string) (all []string, budget int) {
	t.Helper()
	content := strings.Repeat("satır içeriği\n", 60)
	writeFile(t, repo, "memory/note.md", content)
	corpus := seedCorpus(t, repo, 5, 60) // commits note.md too
	all = append(sortedKeys(corpus), "note.md")
	one := estimateFileTokens("topic00/note.md", content)
	return all, one + one/2 // one file per batch, never two
}

// TestRunRollingCursorReachesEveryFile: six files, one per batch, two
// batches a night - every three consecutive nights send every file, for
// three full rotations, whether each night's suggestion is superseded or
// approved.
func TestRunRollingCursorReachesEveryFile(t *testing.T) {
	for _, approve := range []bool{false, true} {
		t.Run(fmt.Sprintf("approve=%v", approve), func(t *testing.T) {
			repo := initKahyaRepo(t)
			all, budget := seedEqualCloudCorpus(t, repo)
			var calls []map[string]string
			c, _, _ := newBatchConsolidator(t, repo, changingRecorder(&calls), nil, func(cfg *Config) {
				cfg.CloudBatchTokenBudget = budget
				cfg.MaxBatchesPerLane = 2
				cfg.ChangedFilesFirst = true
			})
			const rotation = 3 // ceil(6 files / 2 one-file batches a night)
			var nights [][]string
			for n := 1; n <= 3*rotation; n++ {
				calls = nil
				tr := fmt.Sprintf("trace-rotate-%d", n)
				if err := c.Run(context.Background(), tr); err != nil {
					t.Fatalf("night %d Run() error = %v", n, err)
				}
				sent := sentPaths(calls)
				if len(sent) != 2 {
					t.Fatalf("night %d sent %q, want 2 files (one per batch, cap 2)", n, sent)
				}
				nights = append(nights, sent)
				if approve {
					if err := c.Approve(context.Background(), "approve-"+tr); err != nil {
						t.Fatalf("night %d Approve() error = %v", n, err)
					}
				}
			}
			assertEveryWindowSendsAll(t, nights, all, rotation)
		})
	}
}

// TestRunRollingCursorReachesAFileChangedEveryDay: the user edits the file
// that sorts last every day and approves every suggestion. It is "changed"
// every night, and it is sent at least once in every three nights (one
// rotation) like every other file - never stuck behind the carried queue.
func TestRunRollingCursorReachesAFileChangedEveryDay(t *testing.T) {
	repo := initKahyaRepo(t)
	all, budget := seedEqualCloudCorpus(t, repo)
	const edited = "topic04/note.md"
	var calls []map[string]string
	c, _, _ := newBatchConsolidator(t, repo, changingRecorder(&calls), nil, func(cfg *Config) {
		cfg.CloudBatchTokenBudget = budget
		cfg.MaxBatchesPerLane = 2
		cfg.ChangedFilesFirst = true
	})
	const rotation = 3
	var nights [][]string
	for n := 1; n <= 3*rotation; n++ {
		calls = nil
		// Today's user edit (a dirty tree: Run pre-commits it as the user).
		cur := runGit(t, repo, "show", "HEAD:memory/"+edited)
		writeFile(t, repo, "memory/"+edited, cur+fmt.Sprintf("\nkullanıcı notu %d\n", n))
		tr := fmt.Sprintf("trace-edited-%d", n)
		if err := c.Run(context.Background(), tr); err != nil {
			t.Fatalf("night %d Run() error = %v", n, err)
		}
		nights = append(nights, sentPaths(calls))
		if err := c.Approve(context.Background(), "approve-"+tr); err != nil {
			t.Fatalf("night %d Approve() error = %v", n, err)
		}
	}
	assertEveryWindowSendsAll(t, nights, []string{edited}, rotation)
	assertEveryWindowSendsAll(t, nights, all, rotation)
}

// TestRunRollingCursorKeepsFilesTheLocalModelDidNotReach: the local model
// answers one secret-lane batch a night and then goes away. The files it
// did not reach start the next night, so four nights consolidate all four
// files - before, every night started over at the same first file. No
// secret-lane file ever reaches the cloud lane.
func TestRunRollingCursorKeepsFilesTheLocalModelDidNotReach(t *testing.T) {
	repo := initKahyaRepo(t)
	secret := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d"} {
		rel := "finans-" + n + ".md"
		secret[rel] = "TR330006100519786457841326 hesap " + n + "\n" + strings.Repeat("gelir gider\n", 40)
		writeFile(t, repo, "memory/"+rel, secret[rel])
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "seed finans notes")

	var cloudCalls []map[string]string
	var answered []string // the files the local model answered, in order
	callsTonight := 0
	local := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
		callsTonight++
		if callsTonight > 1 {
			return nil, fmt.Errorf("local lane: %w", mlx.ErrLocalModelUnavailable)
		}
		out := map[string]string{}
		for k, v := range files {
			answered = append(answered, k)
			out[k] = v + "YEREL\n"
		}
		return out, nil
	})
	c, _, _ := newBatchConsolidator(t, repo, changingRecorder(&cloudCalls), local, func(cfg *Config) {
		cfg.SecretLaneGlobs = []string{filepath.Join(repo, "memory", "finans*.md")}
		cfg.LocalBatchTokenBudget = estimateFileTokens("finans-a.md", secret["finans-a.md"]) // one file per batch
		cfg.ChangedFilesFirst = true
	})
	for n := 1; n <= 4; n++ {
		callsTonight = 0
		if err := c.Run(context.Background(), fmt.Sprintf("trace-local-gone-%d", n)); err != nil {
			t.Fatalf("night %d Run() error = %v", n, err)
		}
	}
	if want := []string{"finans-a.md", "finans-b.md", "finans-c.md", "finans-d.md"}; !reflect.DeepEqual(answered, want) {
		t.Fatalf("local model answered %q over 4 nights, want each file once, in order %q", answered, want)
	}
	for _, call := range cloudCalls {
		for k := range call {
			if strings.HasPrefix(k, "finans") {
				t.Fatalf("secret-lane file %s reached the cloud lane", k)
			}
		}
	}
}
