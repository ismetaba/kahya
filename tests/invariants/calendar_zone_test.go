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

// dayKeyFormatRe matches a time formatted into a calendar-day or
// calendar-month key.
var dayKeyFormatRe = regexp.MustCompile(`\.Format\(\s*(dayLayout|monthLayout|"2006-01-02"|"2006-01"|"20060102")\s*\)`)

// zonedDayKeyAllowlist names the production day keys whose zone is applied
// on an earlier line: file (repo-relative) -> the code that formats it.
var zonedDayKeyAllowlist = map[string]string{
	// nowLocal := nowInstant.In(loc), one line above.
	"kahyad/internal/briefing/briefing.go": "nowLocal.Format(",
}

// TestCalendarDayKeysNameTheirZone (FIX-FINAL ops-config-r2-timezone-not-
// single-zone, G00 C-22): every production line that formats a calendar
// day or month key applies a zone on that same line (`.In(loc).Format(`).
// A bare now().Format("2006-01-02") keys the day on whatever zone the
// clock value carries - the process's time.Local for time.Now - which is
// how the egress budget, the backup file name and consolidation's "today"
// drifted from the configured `timezone` while the UTC guard above stayed
// green. Tests are exempt.
func TestCalendarDayKeysNameTheirZone(t *testing.T) {
	root, err := RepoRoot("")
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	keys := 0
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
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			for i, line := range strings.Split(string(b), "\n") {
				code := line
				if j := strings.Index(code, "//"); j >= 0 {
					code = code[:j]
				}
				loc := dayKeyFormatRe.FindStringIndex(code)
				if loc == nil {
					continue
				}
				keys++
				if strings.Contains(code[:loc[0]], ".In(") {
					continue
				}
				if allowed, ok := zonedDayKeyAllowlist[rel]; ok && strings.Contains(code, allowed) {
					continue
				}
				t.Errorf("%s:%d formats a calendar day key without naming its zone on the same line (want .In(<the configured zone>).Format(...)): %s", rel, i+1, strings.TrimSpace(line))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if keys < 8 {
		t.Fatalf("found only %d day-key formats under kahyad/ and mcp/ - the pattern no longer matches the tree", keys)
	}
}
