package briefing

import (
	"context"
	"strings"

	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/spawn"
)

// --- test doubles shared across this package's test files ---

// fakeLedger records every LogEvent call, keyed by kind - mirrors
// kahyad/internal/reader/reader_test.go's identically-named/shaped
// helper.
type fakeLedger struct {
	events []fakeEvent
}

type fakeEvent struct {
	traceID string
	kind    string
	payload map[string]any
}

func (l *fakeLedger) LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error {
	l.events = append(l.events, fakeEvent{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (l *fakeLedger) count(kind string) int {
	n := 0
	for _, e := range l.events {
		if e.kind == kind {
			n++
		}
	}
	return n
}

// fakeClassifier is a Classifier double: reports secretLane for any text
// containing one of Marks, unconditionally errors when Err is set (the
// FAIL-CLOSED classifier test's own "classifier cannot run" double), and
// records every text it was asked to classify.
type fakeClassifier struct {
	Marks []string
	Err   error

	Calls []string
}

func (f *fakeClassifier) Classify(ctx context.Context, text string) (secretlane.Verdict, error) {
	f.Calls = append(f.Calls, text)
	if f.Err != nil {
		return secretlane.Verdict{SecretLane: true, Category: secretlane.CategoryUnknown, Reason: "forced_fail_closed"}, f.Err
	}
	for _, m := range f.Marks {
		if m != "" && strings.Contains(text, m) {
			return secretlane.Verdict{SecretLane: true, Category: secretlane.CategoryFinans, Reason: "fake_mark"}, nil
		}
	}
	return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone}, nil
}

// fakeGlobMatcher is a GlobMatcher double: matches every path in Paths
// exactly.
type fakeGlobMatcher struct {
	Paths map[string]bool
}

func (f fakeGlobMatcher) MatchesSecretLane(path string) bool { return f.Paths[path] }

// fakeCalendarRunner returns a canned JSON payload or a canned error
// (ErrCalendarNoAccess in particular - the missing-TCC-grant fallback
// test's own double).
type fakeCalendarRunner struct {
	JSON []byte
	Err  error
}

func (f fakeCalendarRunner) Run(ctx context.Context) ([]byte, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.JSON, nil
}

// fakeGHRunner records every args slice it was asked to run and returns
// a canned response per subcommand ("pr"/"run" - args[0]).
type fakeGHRunner struct {
	PRJSON  []byte
	RunJSON []byte
	Calls   [][]string
}

func (f *fakeGHRunner) Run(ctx context.Context, args []string) ([]byte, error) {
	f.Calls = append(f.Calls, args)
	if len(args) > 0 && args[0] == "pr" {
		return f.PRJSON, nil
	}
	return f.RunJSON, nil
}

// fakeWorkerSpawner records the exact spawn.Envelope it was given (so the
// ordering-invariant test can assert directly against env.Marshal()'s
// JSON bytes - the real wire format) and returns a canned rawJSON summary
// or a canned error.
type fakeWorkerSpawner struct {
	RawJSON string
	Err     error

	Envs []spawn.Envelope
}

func (f *fakeWorkerSpawner) Spawn(ctx context.Context, env spawn.Envelope) (string, error) {
	f.Envs = append(f.Envs, env)
	if f.Err != nil {
		return "", f.Err
	}
	return f.RawJSON, nil
}

// fakeDelivery records every SendNotification call and returns a fixed
// Sent verdict.
type fakeDelivery struct {
	Sent  bool
	Calls []string
}

func (f *fakeDelivery) SendNotification(ctx context.Context, traceID, text string) bool {
	f.Calls = append(f.Calls, text)
	return f.Sent
}

// permissiveClassifier builds a REAL kahyad/internal/secretlane.Classifier
// (deterministic pre-pass live) with a Qwen fallback that reports "not
// secret" for anything the pre-pass alone doesn't already catch -
// standing in for a genuinely warm, working local Qwen server. Tests that
// only care about a SPECIFIC classification decision (an IBAN line, a
// planted glob path, a forced classifier failure) use this so the
// deterministic pre-pass's own real IBAN/TCKN/keyword hits still fire
// authentically, while ordinary text never spuriously fails closed the
// way secretlane.NewClassifier(nil) (no Qwen at all) deliberately does.
func permissiveClassifier() Classifier {
	return secretlane.NewClassifier(secretlane.QwenClassifierFunc(
		func(ctx context.Context, text string) (secretlane.Verdict, error) {
			return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone}, nil
		}))
}

// fakeDedupe is a directly-controllable DedupeChecker double.
type fakeDedupe struct {
	Already bool
	Err     error
}

func (f fakeDedupe) AlreadyDeliveredToday(ctx context.Context, date string) (bool, error) {
	return f.Already, f.Err
}
