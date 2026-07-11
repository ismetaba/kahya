package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
)

// ---- fakes ----

type fakeBudget struct {
	mu sync.Mutex
	m  map[string]int64
}

func newFakeBudget() *fakeBudget { return &fakeBudget{m: map[string]int64{}} }

func (b *fakeBudget) key(host, day string) string { return host + "|" + day }

func (b *fakeBudget) Bytes(_ context.Context, host, day string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m[b.key(host, day)], nil
}

func (b *fakeBudget) Add(_ context.Context, host, day string, n int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := b.key(host, day)
	b.m[k] += n
	return b.m[k], nil
}

type ledgerRow struct {
	traceID string
	kind    string
	payload map[string]any
}

type fakeLedger struct {
	mu   sync.Mutex
	rows []ledgerRow
}

func (l *fakeLedger) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, ledgerRow{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (l *fakeLedger) count(kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.rows {
		if r.kind == kind {
			n++
		}
	}
	return n
}

func (l *fakeLedger) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.rows)
}

type fakeNotifier struct {
	mu      sync.Mutex
	calls   int
	lastMsg string
}

func (n *fakeNotifier) Alarm(_ context.Context, _, _, message string, _ map[string]any) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	n.lastMsg = message
	return nil
}

// testLogger builds a real logx.Logger writing under t.TempDir() so a
// test can read the JSONL file back (this task's own acceptance
// criterion: "every gate decision produces a JSONL log line").
func testLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir := t.TempDir()
	log, err := logx.New(dir, "boot0123456789abcdef0123456789ab")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log, filepath.Join(dir, "kahyad.jsonl")
}

// countJSONLLines counts the JSONL lines in path whose "event" field
// equals event.
func countJSONLLines(t *testing.T, path, event string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["event"] == event {
			n++
		}
	}
	return n
}

func testEgressConfig() policy.EgressConfig {
	return policy.EgressConfig{
		Allowlist: []policy.EgressAllowEntry{
			{Host: "api.anthropic.com"},
			{Host: "xn--mnchen-3ya.de"}, // punycode for "münchen.de"
		},
		DefaultDailyByteBudget: 1024,
	}
}

// ---- CanonicalizeHost ----

func TestCanonicalizeHost_CasePunycodeTrailingDot(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase+trailing dot", "API.Anthropic.com.", "api.anthropic.com"},
		{"punycode decodes to unicode", "XN--MNCHEN-3YA.DE", "münchen.de"},
		{"already unicode, trailing dot", "münchen.de.", "münchen.de"},
		{"bracketed ipv6", "[::1]", "::1"},
		{"ipv6 normalizes to shortened canonical form", "2001:0DB8:0000:0000:0000:0000:0000:0001", "2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalizeHost(tc.in)
			if err != nil {
				t.Fatalf("CanonicalizeHost(%q) error = %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("CanonicalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalizeHost_Empty(t *testing.T) {
	if _, err := CanonicalizeHost("   "); err == nil {
		t.Fatal("expected an error for an empty/whitespace host")
	}
}

// ---- Gate.Check: allowlist ----

func TestCheck_AllowlistNormalization_MatchesAcrossEquivalentForms(t *testing.T) {
	g := NewGate(testEgressConfig(), NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)

	for _, host := range []string{"api.anthropic.com", "API.ANTHROPIC.COM.", "Api.Anthropic.Com"} {
		d, err := g.Check(context.Background(), Target{Host: host, Port: 443}, 10, SessionInfo{TraceID: "t1"})
		if err != nil {
			t.Fatalf("Check(%q) error = %v", host, err)
		}
		if !d.Allow {
			t.Errorf("Check(%q) = deny (%s), want allow", host, d.Reason)
		}
	}

	// The punycode allowlist entry matches its own decoded Unicode form
	// too (both canonicalize to the same string at construction/request
	// time).
	d, err := g.Check(context.Background(), Target{Host: "münchen.de", Port: 443}, 10, SessionInfo{TraceID: "t2"})
	if err != nil {
		t.Fatalf("Check(münchen.de) error = %v", err)
	}
	if !d.Allow {
		t.Errorf("Check(münchen.de) = deny (%s), want allow (punycode allowlist entry equivalence)", d.Reason)
	}
}

func TestCheck_NotInAllowlistDenies(t *testing.T) {
	g := NewGate(testEgressConfig(), NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)

	d, err := g.Check(context.Background(), Target{Host: "evil.example.com", Port: 443}, 10, SessionInfo{TraceID: "t1"})
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	if d.Allow {
		t.Fatal("expected deny for a non-allowlisted host")
	}
	if d.Rule != EventBlockedAllowlist {
		t.Errorf("Rule = %q, want %q", d.Rule, EventBlockedAllowlist)
	}
	if d.Reason == "" {
		t.Error("expected a non-empty Turkish deny reason")
	}
}

// ---- Gate.Check: IP literal / private range rejection ----

func TestCheck_IPLiteralAndPrivateRangeRejectedUnlessAllowlisted(t *testing.T) {
	g := NewGate(testEgressConfig(), NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)

	for _, host := range []string{"192.168.1.1", "169.254.169.254", "10.0.0.5", "8.8.8.8", "127.0.0.1"} {
		d, err := g.Check(context.Background(), Target{Host: host, Port: 443}, 10, SessionInfo{TraceID: "t"})
		if err != nil {
			t.Fatalf("Check(%q) error = %v", host, err)
		}
		if d.Allow {
			t.Errorf("Check(%q) = allow, want deny (no allowlist entry blocks it explicitly, but none permits it either)", host)
		}
	}
}

func TestCheck_ExplicitlyAllowlistedPrivateIPIsAllowed(t *testing.T) {
	cfg := policy.EgressConfig{
		Allowlist: []policy.EgressAllowEntry{
			{Host: "192.168.1.50"}, // operator explicitly allowlisted a LAN device
		},
		DefaultDailyByteBudget: 1024,
	}
	g := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)

	d, err := g.Check(context.Background(), Target{Host: "192.168.1.50", Port: 443}, 10, SessionInfo{TraceID: "t"})
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	if !d.Allow {
		t.Errorf("expected an explicitly allowlisted private IP to be allowed, got deny: %s", d.Reason)
	}
}

// ---- Gate.Check: sensitive-read block ----

func TestCheck_SensitiveReadBlocksNonAllowlistedHost(t *testing.T) {
	sessions := NewSensitiveTracker()
	ledger := &fakeLedger{}
	g := NewGate(testEgressConfig(), sessions, newFakeBudget(), ledger, nil, nil)

	session := SessionInfo{SessionID: "sess-1", TraceID: "t1"}

	// Before marking, a non-allowlisted host is denied for the ordinary
	// allowlist reason.
	d, err := g.Check(context.Background(), Target{Host: "evil.example.com", Port: 443}, 1, session)
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	if d.Rule != EventBlockedAllowlist {
		t.Fatalf("pre-mark Rule = %q, want %q", d.Rule, EventBlockedAllowlist)
	}

	sessions.Mark("sess-1")

	// After marking, the SAME non-allowlisted host is denied for the
	// sensitive reason instead.
	d, err = g.Check(context.Background(), Target{Host: "evil.example.com", Port: 443}, 1, session)
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	if d.Allow {
		t.Fatal("expected deny after sensitive-read mark")
	}
	if d.Rule != EventBlockedSensitive {
		t.Errorf("Rule = %q, want %q", d.Rule, EventBlockedSensitive)
	}

	// An allowlisted host is UNAFFECTED by the sensitive mark (the
	// invariant only hard-blocks ALLOWLIST-EXTERNAL egress).
	d, err = g.Check(context.Background(), Target{Host: "api.anthropic.com", Port: 443}, 1, session)
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	if !d.Allow {
		t.Errorf("expected an allowlisted host to remain reachable after a sensitive-read mark, got deny: %s", d.Reason)
	}

	// The flag never clears: a LATER attempt in the same session still
	// blocks (this task's own acceptance criterion, verbatim).
	d, err = g.Check(context.Background(), Target{Host: "evil.example.com", Port: 443}, 1, session)
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	if d.Allow || d.Rule != EventBlockedSensitive {
		t.Fatalf("expected the sensitive block to persist for the session's lifetime, got Allow=%v Rule=%q", d.Allow, d.Rule)
	}

	if ledger.count(EventBlockedSensitive) != 2 {
		t.Errorf("egress_blocked_sensitive ledger rows = %d, want 2", ledger.count(EventBlockedSensitive))
	}
}

func TestSensitiveTracker_RisesOnlyNeverClears(t *testing.T) {
	tr := NewSensitiveTracker()
	if tr.IsMarked("s1") {
		t.Fatal("expected an unmarked session to report unmarked")
	}
	if changed := tr.Mark("s1"); !changed {
		t.Fatal("expected the first Mark to report changed=true")
	}
	if !tr.IsMarked("s1") {
		t.Fatal("expected s1 to be marked")
	}
	if changed := tr.Mark("s1"); changed {
		t.Fatal("expected a second Mark to report changed=false (already marked)")
	}
	if !tr.IsMarked("s1") {
		t.Fatal("expected s1 to remain marked (rises only, never clears)")
	}
	// A different session is unaffected.
	if tr.IsMarked("s2") {
		t.Fatal("expected s2 to be unaffected by s1's mark")
	}
	// An empty session id is always a no-op.
	if tr.Mark("") {
		t.Fatal("expected Mark(\"\") to be a no-op")
	}
	if tr.IsMarked("") {
		t.Fatal("expected IsMarked(\"\") to always report false")
	}
}

// ---- Gate.Check: budget ----

func TestCheck_BudgetExceededDenies_SecondRequestBlocked(t *testing.T) {
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "budget-test.example.com"}},
		DefaultDailyByteBudget: 1024, // 1 KiB
	}
	notifier := &fakeNotifier{}
	g := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, notifier, nil)

	// First request: 600 bytes, well within the 1 KiB budget.
	d1, err := g.Check(context.Background(), Target{Host: "budget-test.example.com", Port: 443}, 600, SessionInfo{TraceID: "t1"})
	if err != nil {
		t.Fatalf("first Check error = %v", err)
	}
	if !d1.Allow {
		t.Fatalf("first Check = deny (%s), want allow", d1.Reason)
	}

	// Second request: another 600 bytes pushes the cumulative total to
	// 1200 bytes, over the 1 KiB budget -> blocked.
	d2, err := g.Check(context.Background(), Target{Host: "budget-test.example.com", Port: 443}, 600, SessionInfo{TraceID: "t2"})
	if err != nil {
		t.Fatalf("second Check error = %v", err)
	}
	if d2.Allow {
		t.Fatal("expected the second request to be blocked once the daily budget is exceeded")
	}
	if d2.Rule != EventBlockedBudget {
		t.Errorf("Rule = %q, want %q", d2.Rule, EventBlockedBudget)
	}
	if notifier.calls != 1 {
		t.Errorf("notifier.calls = %d, want 1", notifier.calls)
	}
	if !strings.HasPrefix(notifier.lastMsg, "Egress bütçesi aşıldı: ") {
		t.Errorf("notifier message = %q, want the fixed Turkish prefix", notifier.lastMsg)
	}
}

func TestCheck_BudgetRollsOverAtLocalDayBoundary(t *testing.T) {
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "budget-test.example.com"}},
		DefaultDailyByteBudget: 1024,
	}
	g := NewGate(cfg, NewSensitiveTracker(), newFakeBudget(), &fakeLedger{}, nil, nil)

	day1 := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	g.SetClock(func() time.Time { return day1 })

	d1, err := g.Check(context.Background(), Target{Host: "budget-test.example.com", Port: 443}, 900, SessionInfo{TraceID: "t1"})
	if err != nil || !d1.Allow {
		t.Fatalf("day1 first Check: allow=%v err=%v, want allow", d1.Allow, err)
	}
	d2, err := g.Check(context.Background(), Target{Host: "budget-test.example.com", Port: 443}, 900, SessionInfo{TraceID: "t2"})
	if err != nil {
		t.Fatalf("day1 second Check error = %v", err)
	}
	if d2.Allow {
		t.Fatal("expected day1's second request to be blocked (900+900 > 1024)")
	}

	// Advance the clock past local midnight - a new day's counter starts
	// at zero regardless of how much the PREVIOUS day consumed.
	day2 := day1.Add(2 * time.Hour)
	g.SetClock(func() time.Time { return day2 })

	d3, err := g.Check(context.Background(), Target{Host: "budget-test.example.com", Port: 443}, 900, SessionInfo{TraceID: "t3"})
	if err != nil || !d3.Allow {
		t.Fatalf("day2 first Check: allow=%v err=%v, want allow (budget rolled over)", d3.Allow, err)
	}
}

// ---- Gate.Check: every decision ledgered + logged ----

func TestCheck_EveryDecisionLedgeredAndLogged(t *testing.T) {
	cfg := policy.EgressConfig{
		Allowlist:              []policy.EgressAllowEntry{{Host: "api.anthropic.com"}, {Host: "budget-test.example.com"}},
		DefaultDailyByteBudget: 100,
	}
	ledger := &fakeLedger{}
	log, jsonlPath := testLogger(t)
	sessions := NewSensitiveTracker()
	g := NewGate(cfg, sessions, newFakeBudget(), ledger, nil, log)

	// Scripted sequence: allow, deny (allowlist), deny (sensitive), deny
	// (budget).
	mustCheck := func(host string, nbytes int64, session SessionInfo) Decision {
		d, err := g.Check(context.Background(), Target{Host: host, Port: 443}, nbytes, session)
		if err != nil {
			t.Fatalf("Check(%q) error = %v", host, err)
		}
		return d
	}

	if d := mustCheck("api.anthropic.com", 10, SessionInfo{TraceID: "t1"}); !d.Allow {
		t.Fatalf("expected allow, got deny: %s", d.Reason)
	}
	if d := mustCheck("not-allowlisted.example.com", 10, SessionInfo{TraceID: "t2"}); d.Allow {
		t.Fatal("expected deny (allowlist)")
	}
	sessions.Mark("sess-x")
	if d := mustCheck("not-allowlisted.example.com", 10, SessionInfo{SessionID: "sess-x", TraceID: "t3"}); d.Allow {
		t.Fatal("expected deny (sensitive)")
	}
	if d := mustCheck("budget-test.example.com", 1000, SessionInfo{TraceID: "t4"}); d.Allow {
		t.Fatal("expected deny (budget)")
	}

	// One events row per decision.
	if got, want := ledger.total(), 4; got != want {
		t.Fatalf("ledger.total() = %d, want %d", got, want)
	}
	if ledger.count(EventAllowed) != 1 {
		t.Errorf("EventAllowed rows = %d, want 1", ledger.count(EventAllowed))
	}
	if ledger.count(EventBlockedAllowlist) != 1 {
		t.Errorf("EventBlockedAllowlist rows = %d, want 1", ledger.count(EventBlockedAllowlist))
	}
	if ledger.count(EventBlockedSensitive) != 1 {
		t.Errorf("EventBlockedSensitive rows = %d, want 1", ledger.count(EventBlockedSensitive))
	}
	if ledger.count(EventBlockedBudget) != 1 {
		t.Errorf("EventBlockedBudget rows = %d, want 1", ledger.count(EventBlockedBudget))
	}

	// Every ledger row carries the request's own trace_id.
	wantTraceIDs := map[string]bool{"t1": true, "t2": true, "t3": true, "t4": true}
	for _, r := range ledger.rows {
		if !wantTraceIDs[r.traceID] {
			t.Errorf("unexpected trace_id on ledger row: %q", r.traceID)
		}
	}

	// And a matching JSONL line for each event kind (this task's own
	// acceptance criterion: "produces a JSONL log line").
	for _, kind := range []string{EventAllowed, EventBlockedAllowlist, EventBlockedSensitive, EventBlockedBudget} {
		if n := countJSONLLines(t, jsonlPath, kind); n != 1 {
			t.Errorf("JSONL lines for event=%s = %d, want 1", kind, n)
		}
	}
}

// ---- Budget persistence across a kahyad restart ----

func TestSQLBudget_PersistsAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "brain.db")

	st1, err := store.Open(config.Config{DBPath: dbPath})
	if err != nil {
		t.Fatalf("store.Open (first): %v", err)
	}
	budget1 := NewSQLBudget(st1.Queries)
	total, err := budget1.Add(context.Background(), "restart-test.example.com", "2026-01-01", 500)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if total != 500 {
		t.Fatalf("total = %d, want 500", total)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	// Simulate a kahyad restart: reopen the SAME database file.
	st2, err := store.Open(config.Config{DBPath: dbPath})
	if err != nil {
		t.Fatalf("store.Open (second): %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	budget2 := NewSQLBudget(st2.Queries)

	got, err := budget2.Bytes(context.Background(), "restart-test.example.com", "2026-01-01")
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if got != 500 {
		t.Fatalf("Bytes after restart = %d, want 500 (persisted row)", got)
	}

	// Assert the row itself, directly (this task's own acceptance
	// criterion: "counter survives restart (persisted row asserted)").
	var bytes int64
	if err := st2.DB().QueryRow(
		`SELECT bytes FROM egress_budget WHERE host = ? AND day = ?`, "restart-test.example.com", "2026-01-01",
	).Scan(&bytes); err != nil {
		t.Fatalf("query egress_budget row: %v", err)
	}
	if bytes != 500 {
		t.Fatalf("egress_budget.bytes = %d, want 500", bytes)
	}
}
