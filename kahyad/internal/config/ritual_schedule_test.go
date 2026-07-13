package config

import "testing"

// findJob returns the CalendarSpec of the named cfg.Jobs entry (fatal if
// absent).
func jobCalendar(t *testing.T, cfg Config, name string) CalendarSpec {
	t.Helper()
	for _, j := range cfg.Jobs {
		if j.Name == name {
			return j.Calendar
		}
	}
	t.Fatalf("job %q not found in cfg.Jobs", name)
	return CalendarSpec{}
}

// TestRitualAndBriefingConfigKeysMoveTheScheduledJob is the regression test
// for the W5-03 review MAJOR: ritual_weekly_* / briefing_hour/minute were
// parsed into Config fields but never rebuilt the truth-ritual /
// morning-briefing job CalendarSpec, so an operator override silently did
// nothing to the actual schedule.
func TestRitualAndBriefingConfigKeysMoveTheScheduledJob(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfigYAML(t, home, "ritual_weekly_weekday: 3\nritual_weekly_hour: 9\nritual_weekly_minute: 15\nbriefing_hour: 7\nbriefing_minute: 5\n")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	rc := jobCalendar(t, cfg, "truth-ritual")
	if rc.Weekday == nil || *rc.Weekday != 3 || rc.Hour == nil || *rc.Hour != 9 || rc.Minute == nil || *rc.Minute != 15 {
		t.Errorf("truth-ritual calendar = %+v, want weekday=3 hour=9 minute=15 (ritual_weekly_* must move the job)", rc)
	}

	bc := jobCalendar(t, cfg, "morning-briefing")
	if bc.Hour == nil || *bc.Hour != 7 || bc.Minute == nil || *bc.Minute != 5 {
		t.Errorf("morning-briefing calendar = %+v, want hour=7 minute=5 (briefing_hour/minute must move the job)", bc)
	}
}
