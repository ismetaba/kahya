package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readinessHandler serves a canned GET /readiness body (mirrors
// kahyad/internal/readiness.Report).
func readinessHandler(t *testing.T, body string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readiness", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	return mux
}

// allGreenBuildGates is a Report body whose three build gates are green and
// whose usage gates are red (empty window) - the state right at task time.
const readinessGreenBuildRedUsage = `{
  "build_gates": {
    "retrieval": {"green": true, "age_hours": 1.0}, "precision": 0.9,
    "redteam": {"green": true, "age_hours": 1.0}, "bypasses": 0,
    "restore_drill": {"green": true, "age_hours": 1.0}, "restore_ok": true
  },
  "usage_gates": {
    "commands_per_day_ok": false, "commands_per_day": 0,
    "remembered_ok": false, "remembered_per_week": 0,
    "window_ok": false, "window_days": 0,
    "data_loss_ok": null
  },
  "northstar": {
    "clarification_turn_rate": null, "clarification_ok": null,
    "palette_first_token_p50_ms": null, "palette_ok": null
  }
}`

// readinessRedBuild has a missing/red retrieval gate ("veri yok").
const readinessRedBuild = `{
  "build_gates": {
    "retrieval": {"green": false, "reason": "veri yok", "age_hours": null}, "precision": null,
    "redteam": {"green": true, "age_hours": 1.0}, "bypasses": 0,
    "restore_drill": {"green": true, "age_hours": 1.0}, "restore_ok": true
  },
  "usage_gates": {"commands_per_day_ok": false, "remembered_ok": false, "window_ok": false, "data_loss_ok": null},
  "northstar": {"clarification_turn_rate": null, "palette_first_token_p50_ms": null}
}`

// readinessGreenUsage has green build gates AND a green sustained usage window.
const readinessGreenUsage = `{
  "build_gates": {
    "retrieval": {"green": true, "age_hours": 1.0}, "precision": 0.9,
    "redteam": {"green": true, "age_hours": 1.0}, "bypasses": 0,
    "restore_drill": {"green": true, "age_hours": 1.0}, "restore_ok": true
  },
  "usage_gates": {
    "commands_per_day_ok": true, "commands_per_day": 12.0,
    "remembered_ok": true, "remembered_per_week": 7.0,
    "window_ok": true, "window_days": 14,
    "data_loss_ok": null
  },
  "northstar": {
    "clarification_turn_rate": 0.3, "clarification_ok": true,
    "palette_first_token_p50_ms": 900, "palette_ok": true
  }
}`

// readinessGreenUsageRedNorthStar has green build gates + a green §9 usage
// window but FAILING north-star targets (clarification-turn rate above 40%,
// p50 above 1.5s). §9 is the contract: north-star is REPORTED, never gating.
const readinessGreenUsageRedNorthStar = `{
  "build_gates": {
    "retrieval": {"green": true, "age_hours": 1.0}, "precision": 0.9,
    "redteam": {"green": true, "age_hours": 1.0}, "bypasses": 0,
    "restore_drill": {"green": true, "age_hours": 1.0}, "restore_ok": true
  },
  "usage_gates": {
    "commands_per_day_ok": true, "commands_per_day": 12.0,
    "remembered_ok": true, "remembered_per_week": 7.0,
    "window_ok": true, "window_days": 14,
    "data_loss_ok": null
  },
  "northstar": {
    "clarification_turn_rate": 0.55, "clarification_ok": false,
    "palette_first_token_p50_ms": 2000, "palette_ok": false
  }
}`

// TestReadinessStartGreen proves --phase=start exits 0 when all build gates are
// green (usage gates being red does not matter for start).
func TestReadinessStartGreen(t *testing.T) {
	sock := startFakeServer(t, readinessHandler(t, readinessGreenBuildRedUsage))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"readiness", "--phase=start"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, MsgReadinessStartGreen) {
		t.Errorf("missing start-green line:\n%s", out)
	}
	if !strings.Contains(out, MsgReadinessPass) {
		t.Errorf("expected GEÇTİ in build table:\n%s", out)
	}
}

// TestReadinessStartRedExitsNonZero proves a red/missing build gate exits
// non-zero at --phase=start.
func TestReadinessStartRedExitsNonZero(t *testing.T) {
	sock := startFakeServer(t, readinessHandler(t, readinessRedBuild))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"readiness"}, strings.NewReader(""), &stdout, &stderr) // default phase=start
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero for a red build gate")
	}
	out := stdout.String()
	if !strings.Contains(out, "veri yok") {
		t.Errorf("expected 'veri yok' reason in output:\n%s", out)
	}
	if !strings.Contains(out, MsgReadinessStartRed) {
		t.Errorf("expected start-red line:\n%s", out)
	}
}

// TestReadinessCompleteRedAtTaskTime proves --phase=complete is red when the
// usage window is empty, even with green build gates - the EXPECTED state at
// task time.
func TestReadinessCompleteRedAtTaskTime(t *testing.T) {
	sock := startFakeServer(t, readinessHandler(t, readinessGreenBuildRedUsage))
	t.Setenv("KAHYA_SOCKET", sock)

	// A dogfood file with NO data-loss incident, so data-loss is green - the
	// redness must come purely from the usage window.
	dogfood := writeDogfood(t, "| 2026-07-13 | 12 | 1 | type: crash panel çöktü |\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"readiness", "--phase=complete", "--dogfood", dogfood}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero (usage window empty at task time)")
	}
	if !strings.Contains(stdout.String(), MsgReadinessCompleteRed) {
		t.Errorf("expected complete-red line:\n%s", stdout.String())
	}
}

// TestReadinessCompleteGreenWithWindow proves --phase=complete exits 0 when the
// build gates are green, the metrics window satisfies the §9 usage gates, and
// docs/dogfood.md has zero data-loss incidents (the green path).
func TestReadinessCompleteGreenWithWindow(t *testing.T) {
	sock := startFakeServer(t, readinessHandler(t, readinessGreenUsage))
	t.Setenv("KAHYA_SOCKET", sock)
	dogfood := writeDogfood(t, "| 2026-07-13 | 12 | 1 | — |\n| 2026-07-14 | 11 | 2 | type: safety yanlış onay |\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"readiness", "--phase=complete", "--dogfood", dogfood}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)\n%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), MsgReadinessCompleteGreen) {
		t.Errorf("expected complete-green line:\n%s", stdout.String())
	}
}

// TestReadinessCompleteGreenDespiteRedNorthStar locks the "§9 is the contract"
// rule: with green build + green §9 usage gates, --phase=complete exits 0 EVEN
// WHEN the north-star targets (clarification-turn rate, palette→first-token
// p50) are failing. North-star is reported, never gating - a regression that
// started gating on it would flip this test red.
func TestReadinessCompleteGreenDespiteRedNorthStar(t *testing.T) {
	sock := startFakeServer(t, readinessHandler(t, readinessGreenUsageRedNorthStar))
	t.Setenv("KAHYA_SOCKET", sock)
	dogfood := writeDogfood(t, "| 2026-07-13 | 12 | 1 | — |\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"readiness", "--phase=complete", "--dogfood", dogfood}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: a failing north-star must NOT gate --phase=complete (§9 is the contract)\nstderr=%q\n%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), MsgReadinessCompleteGreen) {
		t.Errorf("expected complete-green line despite red north-star:\n%s", stdout.String())
	}
}

// TestReadinessCompleteDataLossRed proves a type: data-loss incident in
// docs/dogfood.md flips --phase=complete to red even when every other gate is
// green.
func TestReadinessCompleteDataLossRed(t *testing.T) {
	sock := startFakeServer(t, readinessHandler(t, readinessGreenUsage))
	t.Setenv("KAHYA_SOCKET", sock)
	dogfood := writeDogfood(t, "| 2026-07-16 | 13 | 1 | type: data-loss episodes tablosu boşaldı |\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"readiness", "--phase=complete", "--dogfood", dogfood}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero: a data-loss incident must fail the gate")
	}
	if !strings.Contains(stdout.String(), MsgReadinessCompleteRed) {
		t.Errorf("expected complete-red line:\n%s", stdout.String())
	}
}

// TestReadinessCompleteMissingDogfoodFailClosed proves an unreadable dogfood
// file fails the data-loss gate closed.
func TestReadinessCompleteMissingDogfoodFailClosed(t *testing.T) {
	sock := startFakeServer(t, readinessHandler(t, readinessGreenUsage))
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"readiness", "--phase=complete", "--dogfood", filepath.Join(t.TempDir(), "absent.md")},
		strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero: a missing dogfood file must fail closed")
	}
}

// TestReadinessDaemonDown proves an unreachable daemon yields a non-zero exit
// and the Turkish daemon-error string, with NO direct-db fallback.
func TestReadinessDaemonDown(t *testing.T) {
	sock := t.TempDir() + "/nope.sock"
	t.Setenv("KAHYA_SOCKET", sock)

	var stdout, stderr bytes.Buffer
	code := run([]string{"readiness"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero when daemon is down")
	}
	want := fmt.Sprintf(MsgDaemonUnreachable, sock)
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty (no table on failure)", stdout.String())
	}
}

// TestReadinessBadPhase proves an unknown --phase is rejected locally.
func TestReadinessBadPhase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"readiness", "--phase=bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 for a bad phase", code)
	}
	if !strings.Contains(stderr.String(), MsgReadinessUsage) {
		t.Errorf("stderr = %q, want the usage line", stderr.String())
	}
}

func writeDogfood(t *testing.T, rows string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dogfood.md")
	body := "| Gün | Komut | Hatırladı | Olay |\n| --- | --- | --- | --- |\n" + rows
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write dogfood fixture: %v", err)
	}
	return path
}
