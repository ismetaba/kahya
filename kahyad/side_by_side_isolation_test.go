package main

import (
	"regexp"
	"strings"
	"testing"
)

// xmlCommentRE matches one <!-- ... --> comment (non-greedy, multi-line).
var xmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)

// codeOnly drops shell comment lines (.sh) or XML comments (.plist) so a
// test sees only what the file really sets.
func codeOnly(rel, src string) string {
	if strings.HasSuffix(rel, ".plist") {
		return xmlCommentRE.ReplaceAllString(src, "")
	}
	var keep []string
	for _, l := range strings.Split(src, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// realHomeSideBySideBoots are the files that boot a real-HOME, non-prod
// kahyad beside prod: the dev profile's provisioning script and launchd
// plist, and the restore drill.
var realHomeSideBySideBoots = []string{
	"scripts/kahya-dev-env.sh",
	"dev/launchd/com.kahya.dev.plist",
	"scripts/restore-drill.sh",
}

// TestRealHomeSideBySideProfilesNeverCarryDevOnlyOverrides (FIX-FINAL
// adv2-dev-profile-side-by-side-shares-prod-bot-anchor-jobs): a non-prod
// daemon keeps prod's Telegram identity only under the dev-only token
// override (config.Load) - which exists for the hermetic temp-HOME gates.
// The real-HOME side-by-side boots must never set it, or the dev daemon
// would long-poll prod's bot again.
func TestRealHomeSideBySideProfilesNeverCarryDevOnlyOverrides(t *testing.T) {
	for _, rel := range realHomeSideBySideBoots {
		src := codeOnly(rel, readRepoFile(t, rel))
		for _, override := range []string{telegramTokenOverrideEnvVar} {
			if strings.Contains(src, override) {
				t.Errorf("%s sets %s: a real-HOME %s daemon would act on prod's identity", rel, override, "side-by-side")
			}
		}
	}
}
