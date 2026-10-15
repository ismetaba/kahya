package store

import (
	"context"
	"testing"
)

// TestListIngestDoneBySHA256BindsTheHash (G13 MEM-6): the ingest duplicate
// check reads only ingest.done rows whose payload names that content hash,
// newest first (the json_extract parameter is bound - the sqlc v1.30.0
// parameter-dropping gotcha).
func TestListIngestDoneBySHA256BindsTheHash(t *testing.T) {
	ctx := context.Background()
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, ev := range []struct {
		kind, sha, file string
	}{
		{"ingest.done", "aaa", "ingest/a.md"},
		{"ingest.done", "bbb", "ingest/b.md"},
		{"ingest.failed", "aaa", ""},
		{"ingest.done", "aaa", "ingest/a-2.md"},
	} {
		if err := s.LogEvent(ctx, "tr", ev.kind, map[string]any{"sha256": ev.sha, "file": ev.file}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Queries.ListIngestDoneBySHA256(ctx, "aaa")
	if err != nil || len(rows) != 2 || rows[0].ID < rows[1].ID {
		t.Fatalf("ListIngestDoneBySHA256(aaa) = %+v, %v; want the two ingest.done rows, newest first", rows, err)
	}
	if rows, err := s.Queries.ListIngestDoneBySHA256(ctx, "ccc"); err != nil || len(rows) != 0 {
		t.Fatalf("ListIngestDoneBySHA256(ccc) = %+v, %v; want none", rows, err)
	}
}
