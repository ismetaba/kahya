package consolidation

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/backup"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%s): %v", name, err)
	}
	return loc
}

// TestRunDayFollowsTheConfiguredZone (FIX-FINAL ops-config-r2-timezone-
// not-single-zone): the run's "today" - the midnight the user-touched
// window starts at and the branch date - is the configured zone's day,
// not the clock's. 22:30 UTC on the 12th is already the 13th in Kiritimati
// (UTC+14) and still the 12th in Pago Pago (UTC-11).
func TestRunDayFollowsTheConfiguredZone(t *testing.T) {
	instant := time.Date(2026, 7, 12, 22, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		loc          *time.Location
		wantBranch   string
		wantMidnight time.Time
	}{
		{"unset keeps the clock's own zone", nil, "20260712", time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)},
		{"UTC", time.UTC, "20260712", time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)},
		{"Pacific/Kiritimati", mustZone(t, "Pacific/Kiritimati"), "20260713", time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)},
		{"Pacific/Pago_Pago", mustZone(t, "Pacific/Pago_Pago"), "20260712", time.Date(2026, 7, 12, 11, 0, 0, 0, time.UTC)},
		{"Europe/Istanbul", mustZone(t, "Europe/Istanbul"), "20260713", time.Date(2026, 7, 12, 21, 0, 0, 0, time.UTC)},
	} {
		local, midnight, branch := runDay(instant, tc.loc)
		if !local.Equal(instant) {
			t.Errorf("%s: local = %v, want the same instant as %v", tc.name, local, instant)
		}
		if !midnight.Equal(tc.wantMidnight) {
			t.Errorf("%s: midnight = %v, want %v", tc.name, midnight.UTC(), tc.wantMidnight)
		}
		if branch != ConsolidationBranchPrefix+tc.wantBranch {
			t.Errorf("%s: branch = %q, want %q", tc.name, branch, ConsolidationBranchPrefix+tc.wantBranch)
		}
	}
}

// TestRunNamesItsBranchAfterTheConfiguredZonesDay: Run itself uses runDay -
// the pending suggestion's branch carries the configured zone's date.
func TestRunNamesItsBranchAfterTheConfiguredZonesDay(t *testing.T) {
	for _, tc := range []struct {
		name string
		loc  *time.Location
		want string
	}{
		{"unset", nil, "20260712"},
		{"Pacific/Kiritimati", mustZone(t, "Pacific/Kiritimati"), "20260713"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initKahyaRepo(t)
			writeFile(t, repo, "memory/note.md", "line one\nline two\nline three\n")
			cloud := SessionFunc(func(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
				out := map[string]string{}
				for k, v := range files {
					out[k] = v + "\nMARKER\n"
				}
				return out, nil
			})
			logger := &fakeEventStore{}
			c := &Consolidator{
				Cfg: Config{
					KahyaDir: repo, MemoryDir: filepath.Join(repo, "memory"), WorktreeParentDir: t.TempDir(),
					Now:      func() time.Time { return time.Date(2026, 7, 12, 22, 30, 0, 0, time.UTC) },
					Location: tc.loc,
				},
				Git:         backup.NewExecGitRunner(),
				Cloud:       cloud,
				EventLogger: logger,
				EventReader: logger,
			}
			if err := c.Run(context.Background(), "trace-zone"); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			pending, err := FindPending(context.Background(), logger)
			if err != nil || pending == nil {
				t.Fatalf("FindPending() = (%+v, %v), want a pending suggestion", pending, err)
			}
			if pending.Branch != ConsolidationBranchPrefix+tc.want {
				t.Fatalf("pending branch = %q, want %q", pending.Branch, ConsolidationBranchPrefix+tc.want)
			}
			if list := runGit(t, repo, "branch", "--list", ConsolidationBranchPrefix+"*"); !strings.Contains(list, tc.want) {
				t.Fatalf("branch --list = %q, want the %s branch", list, tc.want)
			}
		})
	}
}
