package invariants

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// utcDayKeyRe matches a timestamp turned into a UTC calendar-day or
// calendar-month key: t.UTC().Format("2006-01-02"), .UTC().Format(dayLayout)
// and their month forms.
var utcDayKeyRe = regexp.MustCompile(`UTC\(\)\s*\.Format\(\s*(dayLayout|monthLayout|"2006-01-02"|"2006-01")\s*\)`)

// sqliteLocaltimeRe matches SQLite's 'localtime' modifier, which buckets by
// whatever TZ the process happens to run under.
var sqliteLocaltimeRe = regexp.MustCompile(`'localtime'`)

// TestNoUTCCalendarDayKeysInProductionCode (FIX-U-1, G00 C-22): every
// "which calendar day is it" decision in kahyad uses the ONE configured
// zone (config.Config.Location(), config key `timezone`) - the briefing's
// once-per-day gate, metrics/readiness day buckets, the cost governor's
// daily/monthly USD buckets, the memory inbox's day file. No production Go
// file may key a day or month on UTC, or bucket days with SQLite's
// process-TZ 'localtime' modifier. Ledger timestamps themselves stay UTC
// instants (RFC3339Nano); only day KEYS are covered. Tests are exempt.
func TestNoUTCCalendarDayKeysInProductionCode(t *testing.T) {
	root, err := RepoRoot("")
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	scanned := 0
	for _, top := range []string{"kahyad", "mcp"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			scanned++
			rel, _ := filepath.Rel(root, p)
			for i, line := range strings.Split(string(b), "\n") {
				code := line
				if j := strings.Index(code, "//"); j >= 0 {
					code = code[:j] // comments may describe the old behavior
				}
				if utcDayKeyRe.MatchString(code) {
					t.Errorf("%s:%d keys a calendar day/month on UTC (use the configured zone, cfg.Location()): %s", rel, i+1, strings.TrimSpace(line))
				}
				if sqliteLocaltimeRe.MatchString(code) {
					t.Errorf("%s:%d buckets days with SQLite 'localtime' (process TZ); bucket in Go with the configured zone: %s", rel, i+1, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d production Go files under kahyad/ and mcp/ - the walk is broken", scanned)
	}
}
