package w3gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/tests/w3/faketelegram"
)

// jsonlRecords decodes every JSONL line of path.
func jsonlRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// hasJSONLEvent reports whether some record has event == kind and every
// want field equal (string compare of the decoded value).
func hasJSONLEvent(recs []map[string]any, kind string, want map[string]string) bool {
	for _, r := range recs {
		if r["event"] != kind {
			continue
		}
		ok := true
		for k, v := range want {
			if s, _ := r[k].(string); !strings.Contains(s, v) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TestDevDaemonBesideProdNeverPollsProdBotOrSyncsJobs (FIX-FINAL
// adv2-dev-profile-side-by-side-shares-prod-bot-anchor-jobs): a real
// KAHYA_ENV=dev daemon whose config.yaml names a Telegram bot and chat (as
// prod's shared config.yaml does for kahya-dev-env.sh's provisioning boot
// and the com.kahya.dev agent) but which carries no dev-only token override
// never sends a single request to that bot - no getMe, no getUpdates
// long-poll competing for prod's callbacks - and syncs no launchd job
// (job_sync_skipped non_prod_profile; the harness launchctl trap would fail
// the test on any launchctl call).
func TestDevDaemonBesideProdNeverPollsProdBotOrSyncsJobs(t *testing.T) {
	fake := faketelegram.New()
	t.Cleanup(fake.Close)
	d := bootKahyad(t, daemonOpts{
		telegramURL: fake.URL(), telegramChatID: gateTelegramChatID, telegramUserID: gateTelegramUserID,
		telegramWithoutOverride: true,
	})
	// The bot is built (and Start would dial getMe) before srv.Run binds
	// the socket bootKahyad waited on; give a would-be poller time anyway.
	time.Sleep(1500 * time.Millisecond)
	if n := fake.Requests(); n != 0 {
		t.Fatalf("the dev daemon sent %d request(s) to the configured Telegram bot, want none", n)
	}
	recs := jsonlRecords(t, filepath.Join(d.homeDir, "kahyad.stderr.log"))
	if !hasJSONLEvent(recs, "telegram_disabled", map[string]string{"reason": "not configured"}) {
		t.Error("no telegram_disabled (not configured) line: a dev daemon without the override must load with no Telegram identity")
	}
	if !hasJSONLEvent(recs, "job_sync_skipped", map[string]string{"reason": "non_prod_profile"}) {
		t.Error("no job_sync_skipped (non_prod_profile) line")
	}
}
