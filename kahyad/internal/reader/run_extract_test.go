package reader

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/mlx"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/secretlane"
)

// G13 MEM-3 phase 1: Runner.RunExtract's lane pipeline (H-7 ordering:
// classification first, local on a secret verdict, never the cloud on any
// failure).

const validFactExtractJSON = `{"entities":[{"name":"Emre","kind":"person","aliases":[]},{"name":"Acme","kind":"org","aliases":[]}],` +
	`"facts":[{"chunk_id":11,"subject":"Emre","predicate":"works_at","object":"Acme","evidentiality":"reported","quote":"Emre bugün Acme'de işe başladığını söyledi.","valid_from":"","valid_to":"","importance":0.7,"retraction":false}]}`

func extractTestChunks() []ExtractChunk {
	return []ExtractChunk{{ID: 11, Text: factChunkA}, {ID: 12, Text: factChunkB}}
}

func verdictClassifier(secret bool, calls *int) *secretlane.Classifier {
	return secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		if calls != nil {
			*calls++
		}
		if secret {
			return secretlane.Verdict{SecretLane: true, Category: secretlane.CategorySaglik, Reason: "qwen"}, nil
		}
		return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone, Reason: "qwen"}, nil
	}))
}

func fatalCloud(t *testing.T) CloudModel {
	return CloudModelFunc(func(ctx context.Context, jobType string, rawBytes []byte, traceID string) (string, error) {
		t.Fatalf("cloud model called (job %s) - secret-lane / unclassifiable extraction must never reach the cloud", jobType)
		return "", nil
	})
}

func TestRunExtractSecretLaneUsesLocalNeverCloud(t *testing.T) {
	local := &fakeLocalModel{response: validFactExtractJSON}
	r := NewRunner(verdictClassifier(true, nil), local, fatalCloud(t), nil, &fakeLedger{})

	out, lane, err := r.RunExtract(context.Background(), extractTestChunks(), testTrace(), false)
	if err != nil {
		t.Fatalf("RunExtract: %v", err)
	}
	if lane != router.LaneSecret || local.calls != 1 {
		t.Fatalf("lane = %q, local.calls = %d; want secret / 1", lane, local.calls)
	}
	if len(out.Facts) != 1 || out.Facts[0].Predicate != "works_at" {
		t.Fatalf("out = %+v", out)
	}
}

func TestRunExtractForceSecretSkipsCloudEvenWhenClassifierSaysNormal(t *testing.T) {
	calls := 0
	local := &fakeLocalModel{response: validFactExtractJSON}
	r := NewRunner(verdictClassifier(false, &calls), local, fatalCloud(t), nil, nil)

	_, lane, err := r.RunExtract(context.Background(), extractTestChunks(), testTrace(), true)
	if err != nil {
		t.Fatalf("RunExtract: %v", err)
	}
	if lane != router.LaneSecret || local.calls != 1 {
		t.Fatalf("lane = %q, local.calls = %d; want secret / 1", lane, local.calls)
	}
	if calls != 0 {
		t.Fatalf("classifier called %d times; forceSecret must not let a normal verdict re-open the cloud lane", calls)
	}
}

func TestRunExtractLocalUnavailableFailsClosed(t *testing.T) {
	for name, local := range map[string]LocalModel{
		"unwired":   nil,
		"no memory": &fakeLocalModel{err: fmt.Errorf("ensure running: %w", mlx.ErrLocalModelUnavailable)},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := &fakeLedger{}
			r := NewRunner(verdictClassifier(true, nil), local, fatalCloud(t), nil, ledger)
			_, lane, err := r.RunExtract(context.Background(), extractTestChunks(), testTrace(), false)
			if !errors.Is(err, ErrLocalUnavailable) {
				t.Fatalf("err = %v, want ErrLocalUnavailable", err)
			}
			if lane != router.LaneSecret {
				t.Fatalf("lane = %q, want secret", lane)
			}
			if ledger.count(EventLocalUnavailable) != 1 {
				t.Fatalf("reader.local_unavailable rows = %d, want 1", ledger.count(EventLocalUnavailable))
			}
		})
	}
}

// erroringVerdictClassifier is a Classifier (the interface, not
// *secretlane.Classifier) that reports an error together with a
// NON-secret verdict - the shape the hardened classify must never route to
// the cloud.
type erroringVerdictClassifier struct{}

func (erroringVerdictClassifier) Classify(ctx context.Context, text string) (secretlane.Verdict, error) {
	return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone, Reason: "qwen"}, errors.New("classifier backend failed")
}

func TestRunExtractClassifierErrorNeverReachesCloud(t *testing.T) {
	local := &fakeLocalModel{response: validFactExtractJSON}
	r := NewRunner(erroringVerdictClassifier{}, local, fatalCloud(t), nil, nil)
	_, lane, err := r.RunExtract(context.Background(), extractTestChunks(), testTrace(), false)
	if err != nil {
		t.Fatalf("RunExtract: %v", err)
	}
	if lane != router.LaneSecret || local.calls != 1 {
		t.Fatalf("lane = %q, local.calls = %d; want secret / 1 (a classification error is never a cloud verdict)", lane, local.calls)
	}
}

// TestRunClassifierErrorWithNonSecretVerdictFailsClosed is the same rule on
// the mail/webpage Run path (red before G13 MEM-3: Run discarded the error
// and took the non-secret verdict to the cloud).
func TestRunClassifierErrorWithNonSecretVerdictFailsClosed(t *testing.T) {
	local := &fakeLocalModel{response: validMailJSON}
	cloud := &countingCloudModel{}
	r := NewRunner(erroringVerdictClassifier{}, local, cloud, nil, nil)
	res, err := r.Run(context.Background(), JobTypeMailSummary, []byte("herhangi bir metin"), testTrace())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Lane != "secret" || cloud.calls != 0 {
		t.Fatalf("lane = %q, cloud.calls = %d; want secret / 0", res.Lane, cloud.calls)
	}
}

func TestRunExtractNormalLaneSendsTheClassifiedBytesToTheCloud(t *testing.T) {
	var classified, sent string
	classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		classified = text
		return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone, Reason: "qwen"}, nil
	}))
	cloud := CloudModelFunc(func(ctx context.Context, jobType string, rawBytes []byte, traceID string) (string, error) {
		if jobType != JobTypeFactExtract {
			t.Errorf("jobType = %q", jobType)
		}
		sent = string(rawBytes)
		return validFactExtractJSON, nil
	})
	local := &fakeLocalModel{}
	r := NewRunner(classifier, local, cloud, nil, nil)

	out, lane, err := r.RunExtract(context.Background(), extractTestChunks(), testTrace(), false)
	if err != nil {
		t.Fatalf("RunExtract: %v", err)
	}
	if lane != router.LaneNormal || local.calls != 0 || len(out.Facts) != 1 {
		t.Fatalf("lane = %q, local.calls = %d, facts = %d", lane, local.calls, len(out.Facts))
	}
	want := RenderExtractInput(extractTestChunks())
	if classified != want || sent != want {
		t.Fatalf("classified %q / sent %q, want both == RenderExtractInput %q", classified, sent, want)
	}
}

func TestRunExtractRejectsInvalidOutputAndLedgers(t *testing.T) {
	bad := strings.Replace(validFactExtractJSON, "Emre bugün Acme'de işe başladığını söyledi.", "Emre Acme'de çalışıyor.", 1)
	for name, content := range map[string]string{"quote not in chunk": bad, "not json": "Emre works at Acme"} {
		t.Run(name, func(t *testing.T) {
			ledger := &fakeLedger{}
			r := NewRunner(verdictClassifier(true, nil), &fakeLocalModel{response: content}, fatalCloud(t), nil, ledger)
			out, _, err := r.RunExtract(context.Background(), extractTestChunks(), testTrace(), false)
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("err = %v, want ErrRejected", err)
			}
			if len(out.Facts) != 0 || len(out.Entities) != 0 {
				t.Fatalf("partial output %+v returned with a rejection", out)
			}
			if ledger.count(EventRejected) != 1 {
				t.Fatalf("reader.rejected rows = %d, want 1", ledger.count(EventRejected))
			}
		})
	}
}

func TestRunExtractAcceptsAFencedObject(t *testing.T) {
	r := NewRunner(verdictClassifier(true, nil), &fakeLocalModel{response: "```json\n" + validFactExtractJSON + "\n```"}, fatalCloud(t), nil, nil)
	out, _, err := r.RunExtract(context.Background(), extractTestChunks(), testTrace(), false)
	if err != nil || len(out.Facts) != 1 {
		t.Fatalf("RunExtract = %+v, %v", out, err)
	}
}

func TestRunExtractRejectsEmptyAndDuplicateChunks(t *testing.T) {
	r := NewRunner(verdictClassifier(true, nil), &fakeLocalModel{response: validFactExtractJSON}, fatalCloud(t), nil, nil)
	if _, _, err := r.RunExtract(context.Background(), nil, testTrace(), false); err == nil {
		t.Fatal("no chunks accepted")
	}
	dup := []ExtractChunk{{ID: 11, Text: "a"}, {ID: 11, Text: "b"}}
	if _, _, err := r.RunExtract(context.Background(), dup, testTrace(), false); err == nil {
		t.Fatal("duplicate chunk ids accepted")
	}
}

// TestRunRefusesFactExtractJobType: fact_extract_v1 has no rawBytes-only
// validator - its only entry point is RunExtract (the citation check needs
// the chunks).
func TestRunRefusesFactExtractJobType(t *testing.T) {
	r := NewRunner(verdictClassifier(true, nil), &fakeLocalModel{response: validFactExtractJSON}, &countingCloudModel{}, nil, nil)
	if _, err := r.Run(context.Background(), JobTypeFactExtract, []byte("x"), testTrace()); err == nil {
		t.Fatal("Run accepted fact_extract_v1")
	}
}

// TestFactExtractCloudModelIsCurrentHaiku pins decision MEM-3.b: the
// extractor's cloud model is the router's current Haiku.
func TestFactExtractCloudModelIsCurrentHaiku(t *testing.T) {
	if factExtractCloudModel != router.ModelHaiku {
		t.Fatalf("factExtractCloudModel = %q, want router.ModelHaiku %q", factExtractCloudModel, router.ModelHaiku)
	}
	if cloudModelFor(JobTypeFactExtract) != router.ModelHaiku {
		t.Fatalf("cloudModelFor(fact_extract_v1) = %q", cloudModelFor(JobTypeFactExtract))
	}
}

func TestWorkerCloudModelFactExtractEnvelopeUsesExtractorModel(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	m := WorkerCloudModel{
		Cmd: []string{"python3", filepath.Join("testdata", "fake_reader_worker.py")},
		OnStderr: func(line string) {
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
		},
	}
	if _, err := m.Read(context.Background(), JobTypeFactExtract, []byte(RenderExtractInput(extractTestChunks())), "trace-extract-envelope"); err != nil {
		t.Fatalf("Read: %v", err)
	}
	mu.Lock()
	diag := strings.Join(lines, "\n")
	mu.Unlock()
	for _, want := range []string{"mode=reader", "schema=" + JobTypeFactExtract, "model=" + router.ModelHaiku} {
		if !strings.Contains(diag, want) {
			t.Errorf("worker diagnostics %q lack %q", diag, want)
		}
	}
}
