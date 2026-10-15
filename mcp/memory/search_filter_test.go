package memory

import (
	"context"
	"errors"
	"testing"
)

// fakeFilteredSearcher is fakeSearcher plus FilteredSearcher.
type fakeFilteredSearcher struct {
	fakeSearcher
	lastFilter SearchFilter
	filtered   int
}

func (f *fakeFilteredSearcher) SearchFiltered(_ context.Context, traceID, q string, k int, sf SearchFilter) ([]Hit, error) {
	f.filtered++
	f.lastTID, f.lastQuery, f.lastK, f.lastFilter = traceID, q, k, sf
	return f.hits, f.err
}

// TestHandleSearchFilterUnsupportedSearcherErrors (FIX-MEM-4): a filtered
// call against a plain Searcher is refused with the byte-exact Turkish
// error, never answered unfiltered.
func TestHandleSearchFilterUnsupportedSearcherErrors(t *testing.T) {
	fs := &fakeSearcher{hits: []Hit{{Path: "a.md", Text: "x"}}}
	s := New(t.TempDir(), fs, &fakeIndexer{}, nil, nil)
	for _, args := range []MemorySearchArgs{
		{Query: "q", Since: "2026-01-01T00:00:00Z"},
		{Query: "q", PathPrefix: "people/"},
		{Query: "q", EntityID: 3},
		{Query: "q", MinTier: "user_asserted"},
		{Query: "q", Source: "external_doc"},
	} {
		_, err := s.HandleSearch(context.Background(), "tr", args)
		if !errors.Is(err, ErrFilterUnsupported) || err.Error() != "memory_search: filtreli arama bu kurulumda kullanılamıyor" {
			t.Fatalf("args %+v: err = %v, want ErrFilterUnsupported", args, err)
		}
	}
	if fs.lastQuery != "" {
		t.Fatalf("the plain Search ran for a filtered call (query %q)", fs.lastQuery)
	}
	// No filter: the plain Search path, unchanged.
	if out, err := s.HandleSearch(context.Background(), "tr", MemorySearchArgs{Query: "q"}); err != nil || len(out.Results) != 1 {
		t.Fatalf("unfiltered call = %+v, %v", out, err)
	}
}

func TestHandleSearchPassesFilterToFilteredSearcher(t *testing.T) {
	fs := &fakeFilteredSearcher{fakeSearcher: fakeSearcher{hits: []Hit{
		{Path: "people/emre.md", Seq: 1, Text: "Emre", Score: 0.5, SourceTier: "user_asserted", EpisodeCreatedAt: "2026-03-01T10:00:00Z", Source: "memory_file"},
	}}}
	s := New(t.TempDir(), fs, &fakeIndexer{}, nil, nil)
	args := MemorySearchArgs{Query: "Emre", K: 4, Since: " 2026-01-01T00:00:00Z ", Until: "2026-06-01T00:00:00+03:00",
		Source: "memory_file", PathPrefix: "people/", EntityID: 7, MinTier: "user_asserted"}
	out, err := s.HandleSearch(context.Background(), "tr-f", args)
	if err != nil {
		t.Fatalf("HandleSearch: %v", err)
	}
	want := SearchFilter{Since: "2026-01-01T00:00:00Z", Until: "2026-06-01T00:00:00+03:00", Source: "memory_file", PathPrefix: "people/", MinTier: "user_asserted", EntityID: 7}
	if fs.filtered != 1 || fs.lastFilter != want || fs.lastK != 4 || fs.lastTID != "tr-f" {
		t.Fatalf("SearchFiltered got %+v (k %d, trace %q, calls %d), want %+v", fs.lastFilter, fs.lastK, fs.lastTID, fs.filtered, want)
	}
	r := out.Results[0]
	if r.EpisodeCreatedAt != "2026-03-01T10:00:00Z" || r.Source != "memory_file" {
		t.Fatalf("result = %+v, want episode_created_at and source echoed", r)
	}
	// A zero filter keeps the plain path even on a FilteredSearcher.
	if _, err := s.HandleSearch(context.Background(), "tr", MemorySearchArgs{Query: "Emre"}); err != nil || fs.filtered != 1 {
		t.Fatalf("an unfiltered call went through SearchFiltered (%d) / %v", fs.filtered, err)
	}
}

// TestHandleSearchRejectsNonRFC3339Since: a since/until that is not RFC3339
// is refused with the Turkish error before any search runs.
func TestHandleSearchRejectsNonRFC3339Since(t *testing.T) {
	fs := &fakeFilteredSearcher{}
	s := New(t.TempDir(), fs, &fakeIndexer{}, nil, nil)
	for _, args := range []MemorySearchArgs{{Query: "q", Since: "2026-01-01"}, {Query: "q", Until: "dün"}} {
		_, err := s.HandleSearch(context.Background(), "tr", args)
		if !errors.Is(err, ErrFilterBadTime) || err.Error() != "memory_search: since/until RFC3339 olmalı" {
			t.Fatalf("args %+v: err = %v, want ErrFilterBadTime", args, err)
		}
	}
	if fs.filtered != 0 {
		t.Fatalf("a search ran for an invalid time (%d calls)", fs.filtered)
	}
}
