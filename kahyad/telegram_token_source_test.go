package main

import (
	"testing"

	"kahya/kahyad/internal/config"
)

// countingTokenReader stands in for the real kahya.telegram Keychain read.
type countingTokenReader struct{ reads int }

func (c *countingTokenReader) Read() (string, error) {
	c.reads++
	return "real-prod-bot-token", nil
}

// TestDevTelegramTokenSourceNeverReadsRealKeychainOutsideProd (FIX-FINAL
// adv2-dev-profile-side-by-side-shares-prod-bot-anchor-jobs): only prod
// reads the real kahya.telegram Keychain item. A dev daemon gets the
// dev-only override or nothing (an error: telegram.New then resolves to a
// disabled bot); every other profile never honors the override and never
// reads the real token. Defense in depth under config.Load, which already
// clears a non-prod profile's Telegram ids.
func TestDevTelegramTokenSourceNeverReadsRealKeychainOutsideProd(t *testing.T) {
	for _, tc := range []struct {
		name, env, override string
		want                string
		wantErr             bool
		wantRealReads       int
	}{
		{"prod", config.EnvProd, "", "real-prod-bot-token", false, 1},
		{"prod_ignores_override", config.EnvProd, "dev-tok", "real-prod-bot-token", false, 1},
		{"dev_override", config.EnvDev, "dev-tok", "dev-tok", false, 0},
		{"dev_without_override", config.EnvDev, "", "", true, 0},
		{"restore_with_override", config.EnvRestore, "dev-tok", "", true, 0},
		{"other_profile", "redteam", "", "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAHYA_TELEGRAM_TOKEN_OVERRIDE", tc.override)
			real := &countingTokenReader{}
			got, err := devTelegramTokenSource{real: real, env: tc.env}.Read()
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("Read() = %q, %v; want %q (error %v)", got, err, tc.want, tc.wantErr)
			}
			if real.reads != tc.wantRealReads {
				t.Errorf("real Keychain reads = %d, want %d", real.reads, tc.wantRealReads)
			}
		})
	}
}
