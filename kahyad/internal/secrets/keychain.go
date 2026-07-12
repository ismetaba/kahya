// Package secrets is kahyad's ONLY read path into the macOS Keychain
// (HANDOFF §4: "Keychain'den bulut anahtarını okuyan tek süreç" — kahyad is
// that one process; this package is how it reads). W0-04 provisioned the
// `kahya.anthropic` Keychain item (ACL'd to the codesigned kahyad binary);
// this package reads it via `/usr/bin/security find-generic-password`,
// caches the value in memory for the process lifetime, and never logs it —
// not even in an error message (a `security` failure carries no secret
// material today, but this package deliberately never risks that changing
// on some future macOS version by including command output in a returned
// error).
package secrets

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// defaultService/defaultAccount match W0-04's provisioning command exactly:
// `security add-generic-password -s kahya.anthropic -a kahya ...`.
const (
	defaultService = "kahya.anthropic"
	defaultAccount = "kahya"
)

// telegramService is W0-04's second provisioned item ("kahya.anthropic /
// kahya.telegram / kahya.anchor Keychain items with -T $(which kahyad)"),
// account "kahya" like every other item this package reads (W3-07: kahyad's
// Telegram bot's BotFather token).
const telegramService = "kahya.telegram"

// anchorService is W0-04's third provisioned item (the same "-T $(which
// kahyad)" comment above names it): the SEPARATE-identity deploy key for
// W4-05's append-only external anchor repo (HANDOFF §5 safety #4 ⚑: "Bu
// kimlik Keychain'de ayrı öğedir, yalnız çapa-yazma kod yolunda okunur" -
// "this identity is a SEPARATE Keychain item, read only by the
// anchor-write code path"). kahyad/internal/anchor's own deploy-key
// accessor is the ONLY caller of NewAnchor in this codebase - that
// package's own import-guard test enforces that permanently.
const anchorService = "kahya.anchor"

// Keychain reads and caches the Anthropic API key from the macOS Keychain.
// Safe for concurrent use.
type Keychain struct {
	mu   sync.Mutex
	key  string
	have bool

	// service/account are unexported so production code always goes
	// through New (the real kahya.anthropic/kahya item); tests construct
	// the struct literal directly to point at a deliberately-missing item
	// without ever touching the real one.
	service string
	account string
}

// New constructs a Keychain reader for the production `kahya.anthropic`
// item (account `kahya`), per W0-04's provisioning command.
func New() *Keychain {
	return &Keychain{service: defaultService, account: defaultAccount}
}

// NewTelegram constructs a Keychain reader for the production
// `kahya.telegram` item (account `kahya`, same W0-04 provisioning
// command/ACL convention as New's `kahya.anthropic`) - kahyad/internal/
// telegram.New's ONLY source for the BotFather token (W3-07). A missing/
// locked item behaves identically to the Anthropic key's own fail path
// (Read returns an error, never cached) - the caller (telegram.New) treats
// that as "bot disabled", never a boot failure.
func NewTelegram() *Keychain {
	return &Keychain{service: telegramService, account: defaultAccount}
}

// NewAnchor constructs a Keychain reader for the production `kahya.anchor`
// item (account `kahya`, same W0-04 provisioning command/ACL convention as
// New's `kahya.anthropic`/NewTelegram's `kahya.telegram`) - the W4-05
// append-only anchor repo's deploy key. Do not call this from anywhere
// other than kahyad/internal/anchor: that package's own
// anchor_import_guard_test.go is the permanent enforcement of the
// Keychain-isolation rule this constructor exists to satisfy (HANDOFF §5
// safety #4 ⚑) - this comment is advisory, the test is the actual guard.
func NewAnchor() *Keychain {
	return &Keychain{service: anchorService, account: defaultAccount}
}

// Read returns the cached Anthropic API key, invoking
// `/usr/bin/security find-generic-password -s <service> -a <account> -w`
// on first call only. A read failure (locked keychain, item absent, or the
// macOS-documented errSecInteractionNotAllowed surfacing as a nonzero exit
// — HANDOFF §7: "Keychain kilitli/erişilemezse ... bulut şeridi fail-fast")
// is returned as an error and is NEVER cached: every subsequent call
// retries the read, so a keychain that unlocks mid-run recovers without a
// daemon restart. Neither the returned key nor the command's stderr is
// ever logged by this package — callers (kahyad/internal/anthproxy) must
// uphold the same discipline for anything derived from it.
func (k *Keychain) Read() (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.have {
		return k.key, nil
	}

	cmd := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", k.service, "-a", k.account, "-w")
	out, err := cmd.Output()
	if err != nil {
		// Deliberately omit `out`/stderr from the error string: even
		// though a `security` failure should never carry secret material,
		// this package never risks a future macOS version changing that
		// assumption and leaking key bytes into a log line.
		return "", fmt.Errorf("secrets: keychain read failed for %s/%s: %w", k.service, k.account, err)
	}
	key := strings.TrimSpace(string(out))
	if key == "" {
		return "", fmt.Errorf("secrets: keychain item %s/%s was empty", k.service, k.account)
	}

	k.key = key
	k.have = true
	return k.key, nil
}
