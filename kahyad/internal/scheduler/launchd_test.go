package scheduler

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/config"
)

func intPtr(v int) *int { return &v }

// TestRenderPlistGolden is the task spec step 7 golden-file test: a fixed
// "smoke" job (Minute:0, any hour — the exact fixture the task's own live
// verification step uses) must render byte-for-byte to testdata/
// smoke.plist.golden.
func TestRenderPlistGolden(t *testing.T) {
	job := config.JobConfig{
		Name:     "smoke",
		Handler:  "smoke",
		Calendar: config.CalendarSpec{Minute: intPtr(0)},
	}
	got, err := RenderPlist(job, "/abs/repo/bin/kahya-trigger", "/abs/home/Library/Logs/Kahya")
	if err != nil {
		t.Fatalf("RenderPlist() error = %v", err)
	}

	want, err := os.ReadFile(filepath.Join("testdata", "smoke.plist.golden"))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("RenderPlist() mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderPlistOmitsUnsetCalendarFields guards StartCalendarInterval's
// documented "absent key = every value of that unit" semantics: a nil
// Hour/Day/Weekday must never render as <key>Hour</key><integer>0</integer>
// (which would instead mean "only at midnight/day 0"), it must be absent
// from the dict entirely.
func TestRenderPlistOmitsUnsetCalendarFields(t *testing.T) {
	job := config.JobConfig{Name: "briefing", Calendar: config.CalendarSpec{Minute: intPtr(30), Hour: intPtr(8)}}
	got, err := RenderPlist(job, "/abs/bin/kahya-trigger", "/abs/logs")
	if err != nil {
		t.Fatalf("RenderPlist() error = %v", err)
	}
	for _, unwanted := range []string{"<key>Day</key>", "<key>Weekday</key>"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("rendered plist contains %q, want it omitted:\n%s", unwanted, got)
		}
	}
	for _, wanted := range []string{"<key>Minute</key>\n\t\t<integer>30</integer>", "<key>Hour</key>\n\t\t<integer>8</integer>"} {
		if !strings.Contains(got, wanted) {
			t.Errorf("rendered plist missing %q:\n%s", wanted, got)
		}
	}
}

// TestRenderPlistEscapesXMLMetacharacters is the MINOR 3 regression test:
// RenderPlist must defensively XML-escape every string value it
// interpolates, even though config.Load's jobNamePattern (DNS-label chars
// only) already rejects a job name containing an XML metacharacter before
// it could ever reach here in the normal Load -> RenderPlist path — belt
// and suspenders, exercised here by calling the exported RenderPlist
// directly with an un-validated JobConfig, bypassing that upstream gate on
// purpose.
func TestRenderPlistEscapesXMLMetacharacters(t *testing.T) {
	job := config.JobConfig{
		Name:     `a<b&c"d'e`,
		Handler:  "smoke",
		Calendar: config.CalendarSpec{Minute: intPtr(0)},
	}
	got, err := RenderPlist(job, "/abs/bin/kahya-trigger", "/abs/logs")
	if err != nil {
		t.Fatalf("RenderPlist() error = %v", err)
	}

	// None of the raw metacharacters may appear unescaped in the output.
	for _, bad := range []string{"<b&c", `"d'e`, "a<b", `d'e"`} {
		if strings.Contains(got, bad) {
			t.Errorf("RenderPlist() output contains an unescaped metacharacter sequence %q:\n%s", bad, got)
		}
	}
	// The escaped form must still be present (proves the name round-trips,
	// rather than being silently dropped).
	if !strings.Contains(got, "a&lt;b&amp;c&#34;d&#39;e") {
		t.Errorf("RenderPlist() output does not contain the expected escaped job name:\n%s", got)
	}

	// The output must still be well-formed XML (a DOCTYPE-bearing plist,
	// which encoding/xml's tokenizer accepts as a Directive token).
	dec := xml.NewDecoder(strings.NewReader(got))
	for {
		if _, err := dec.Token(); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("RenderPlist() output is not well-formed XML: %v\n%s", err, got)
		}
	}
}

// fakeRunner records every launchctl invocation Sync makes, without ever
// shelling out for real.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (f *fakeRunner) Run(args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]string(nil), args...)
	f.calls = append(f.calls, cp)
	return nil
}

func (f *fakeRunner) countPrefix(word string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == word {
			n++
		}
	}
	return n
}

func testSyncOptions(t *testing.T) SyncOptions {
	t.Helper()
	return SyncOptions{
		LaunchAgentsDir: t.TempDir(),
		JobLogDir:       t.TempDir(),
		TriggerBinPath:  "/abs/bin/kahya-trigger",
	}
}

// TestSyncInstallsNewJobThenIsIdempotent guards task spec step 5's two
// halves: a new job gets its plist written + bootout(ignored)+bootstrap
// exactly once, and re-running Sync with the SAME job makes NO further
// launchctl calls at all (idempotent — no needless bootout/bootstrap
// cycle on an unchanged job).
func TestSyncInstallsNewJobThenIsIdempotent(t *testing.T) {
	opts := testSyncOptions(t)
	jobs := []config.JobConfig{{Name: "smoke", Handler: "smoke", Calendar: config.CalendarSpec{Minute: intPtr(0)}}}
	runner := &fakeRunner{}

	if err := Sync(jobs, opts, runner, nil); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	plistPath := filepath.Join(opts.LaunchAgentsDir, "com.kahya.job.smoke.plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	if got := runner.countPrefix("bootstrap"); got != 1 {
		t.Errorf("bootstrap calls after first Sync = %d, want 1", got)
	}
	if got := runner.countPrefix("bootout"); got != 1 {
		t.Errorf("bootout calls after first Sync = %d, want 1", got)
	}

	// Re-sync with the identical job list: content is byte-identical, so
	// this must be a complete no-op on launchctl.
	if err := Sync(jobs, opts, runner, nil); err != nil {
		t.Fatalf("second Sync() error = %v", err)
	}
	if got := runner.countPrefix("bootstrap"); got != 1 {
		t.Errorf("bootstrap calls after idempotent re-Sync = %d, want still 1", got)
	}
	if got := runner.countPrefix("bootout"); got != 1 {
		t.Errorf("bootout calls after idempotent re-Sync = %d, want still 1", got)
	}
}

// TestSyncRemovesStaleJobWithoutTouchingUnrelatedPlists guards the other
// half of step 5: a job dropped from config gets its plist removed
// (+ bootout), while an unrelated file already in the SAME directory —
// specifically kahyad's OWN com.kahya.kahyad.plist, installed separately
// by `make install-agent` — is never touched.
func TestSyncRemovesStaleJobWithoutTouchingUnrelatedPlists(t *testing.T) {
	opts := testSyncOptions(t)
	runner := &fakeRunner{}

	unrelated := filepath.Join(opts.LaunchAgentsDir, "com.kahya.kahyad.plist")
	if err := os.WriteFile(unrelated, []byte("kahyad's own agent, not ours"), 0o600); err != nil {
		t.Fatalf("seed unrelated plist: %v", err)
	}

	jobs := []config.JobConfig{{Name: "smoke", Handler: "smoke", Calendar: config.CalendarSpec{Minute: intPtr(0)}}}
	if err := Sync(jobs, opts, runner, nil); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	smokePlist := filepath.Join(opts.LaunchAgentsDir, "com.kahya.job.smoke.plist")
	if _, err := os.Stat(smokePlist); err != nil {
		t.Fatalf("smoke plist not written: %v", err)
	}

	// Now drop the job from config and re-sync.
	if err := Sync(nil, opts, runner, nil); err != nil {
		t.Fatalf("Sync() with no jobs error = %v", err)
	}
	if _, err := os.Stat(smokePlist); !os.IsNotExist(err) {
		t.Errorf("stale smoke plist still present after Sync() dropped it: err=%v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated com.kahya.kahyad.plist was removed/touched: %v", err)
	}
	if got := runner.countPrefix("bootout"); got < 2 {
		t.Errorf("bootout calls = %d, want at least 2 (install + removal)", got)
	}
}

// TestSyncCreatesMissingDirectories guards "the sync step creates
// ~/Library/Logs/Kahya/ if missing" (task spec step 2) — and the same for
// LaunchAgentsDir, since a fresh install has neither yet.
func TestSyncCreatesMissingDirectories(t *testing.T) {
	root := t.TempDir()
	opts := SyncOptions{
		LaunchAgentsDir: filepath.Join(root, "LaunchAgents"),
		JobLogDir:       filepath.Join(root, "Logs", "Kahya"),
		TriggerBinPath:  "/abs/bin/kahya-trigger",
	}
	if err := Sync(nil, opts, &fakeRunner{}, nil); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if info, err := os.Stat(opts.LaunchAgentsDir); err != nil || !info.IsDir() {
		t.Errorf("LaunchAgentsDir not created: err=%v", err)
	}
	if info, err := os.Stat(opts.JobLogDir); err != nil || !info.IsDir() {
		t.Errorf("JobLogDir not created: err=%v", err)
	}
}
