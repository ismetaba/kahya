package consolidation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/secretlane"
	"kahya/mcp/memory"
)

// FIX-U-3 review: macOS APFS is case-insensitive but case-preserving. A
// "Gizli/" directory (an agent's write+forget leaves it behind, or the user
// makes it) and the secret-lane dir "gizli/" are ONE directory there, and
// readMemoryFiles reports the stored spelling. Neither a note memory_write
// classified secret nor a secret note the user keeps in a case variant of a
// secret path may land in the CLOUD lane at night.

type caseIndexer struct{}

func (caseIndexer) ReindexFile(context.Context, string, string) (int64, error) { return 1, nil }

// deterministicClassifier is kahyad's production memory_write classifier
// (server.NewMemoryContentClassifier) without the server import.
type deterministicClassifier struct{}

func (deterministicClassifier) ClassifySecretLane(text string) (bool, string) {
	v := secretlane.ClassifyDeterministic(text)
	return v.SecretLane, v.Category
}

type countingEscalator struct{ n int }

func (e *countingEscalator) EscalateSecretLane(context.Context, string, string, string) error {
	e.n++
	return nil
}

const caseIBAN = "Kira için hesap: TR33 0006 1005 1978 6457 8413 26"

// assertNoSecretInCloud fails if any cloud-lane file's content is
// secret-lane by the deterministic classifier.
func assertNoSecretInCloud(t *testing.T, cloud, local map[string]string) {
	t.Helper()
	for rel, content := range cloud {
		if secretlane.ClassifyDeterministic(content).SecretLane {
			t.Errorf("secret-lane note %s partitioned into the CLOUD lane (local = %v)", rel, keys(local))
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPartitionByLaneCaseVariantSecretDirMemoryWriteNeverCloud is the
// review's agent-only chain end to end: memory_write a normal note to
// Gizli/liste.md, memory_forget it (the empty Gizli/ stays), then
// memory_write IBAN content - redirected to gizli/notes/inbox/... With the
// real deterministic classifier and the real doublestar matcher, the
// nightly partition of what is on disk keeps every secret note local: on a
// case-insensitive volume the write is refused outright (its stored name
// Gizli/notes/... is uncovered); on a case-sensitive one it lands
// byte-exact under gizli/.
func TestPartitionByLaneCaseVariantSecretDirMemoryWriteNeverCloud(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	mem := filepath.Join(root, "memory")
	if err := os.MkdirAll(mem, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	globs := []string{filepath.Join(mem, "gizli", "**")}
	esc := &countingEscalator{}
	s := memory.New(mem, nil, caseIndexer{}, nil, nil)
	s.SetSecretLaneSeam(memory.SecretLaneSeam{Classifier: deterministicClassifier{}, Escalator: esc, Dir: "gizli", Globs: globs, Matcher: PolicyGlobMatcher{}})
	ctx := context.Background()

	if _, err := s.HandleWriteForTask(ctx, "t0", "task0", memory.MemoryWriteArgs{Content: "toplanti notu", File: "Gizli/liste.md"}); err != nil {
		t.Fatalf("normal write: %v", err)
	}
	if _, err := s.HandleForget(ctx, "t1", memory.MemoryForgetArgs{File: "Gizli/liste.md"}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	out, writeErr := s.HandleWriteForTask(ctx, "t2", "task2", memory.MemoryWriteArgs{Content: caseIBAN, File: "inbox/2026-10-15.md"})
	if esc.n != 1 {
		t.Errorf("escalations = %d, want 1 (the IBAN write escalates its task either way)", esc.n)
	}

	files, err := readMemoryFiles(mem)
	if err != nil {
		t.Fatalf("readMemoryFiles: %v", err)
	}
	cloud, local := PartitionByLane(files, mem, globs, PolicyGlobMatcher{})
	assertNoSecretInCloud(t, cloud, local)
	if writeErr == nil {
		// Case-sensitive volume: the note is stored byte-exact and local.
		if out.File != "gizli/notes/inbox/2026-10-15.md" {
			t.Fatalf("out.File = %q, want gizli/notes/inbox/2026-10-15.md", out.File)
		}
		if _, ok := local[out.File]; !ok {
			t.Fatalf("local = %v, want %s", keys(local), out.File)
		}
	} else if writeErr.Error() != memory.MsgMemoryWriteSecretLaneRefused {
		t.Fatalf("secret write err = %v, want %q", writeErr, memory.MsgMemoryWriteSecretLaneRefused)
	}
}

// TestPartitionByLaneCaseVariantSecretPathsGoLocal: secret notes the user
// keeps under a case (or Turkish-I) variant of a secret-lane path - files
// readMemoryFiles reports with their stored spelling - are local-lane under
// the real policy.yaml-shaped globs; ordinary notes stay cloud.
func TestPartitionByLaneCaseVariantSecretPathsGoLocal(t *testing.T) {
	const memDir = "/Users/u/Kahya/memory"
	globs := []string{"/Users/u/Kahya/memory/finans*.md", "/Users/u/Kahya/memory/gizli/**"}
	files := map[string]string{
		"Gizli/banka.md":          "user secret",
		"GIZLI/notes/x.md":        "user secret",
		"GİZLİ/y.md":              "user secret",
		"Finans-2026.md":          "user secret",
		"FINANS.md":               "user secret",
		"notes/genel.md":          "ordinary",
		"Inbox/2026-10-15.md":     "ordinary",
		"notes/Gizli-toplanti.md": "ordinary",
	}
	cloud, local := PartitionByLane(files, memDir, globs, PolicyGlobMatcher{})
	for rel, content := range files {
		_, isLocal := local[rel]
		if wantLocal := content == "user secret"; isLocal != wantLocal {
			t.Errorf("%s: local = %v, want %v (cloud = %v)", rel, isLocal, wantLocal, keys(cloud))
		}
	}
}
