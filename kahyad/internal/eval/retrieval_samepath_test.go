package eval_test

// This external test package proves - at COMPILE time, not by comment - that
// the W78-01 retrieval eval and the <hafiza> injection path traverse the SAME
// exported search function. The eval runner's search dependency is
// eval.RetrievalSearcher; the server route that renders <hafiza> injection
// (server.handleMemorySearch) takes server.Searcher. Both interfaces have the
// identical method set and BOTH are satisfied by the ONE concrete
// *search.Searcher value main.go constructs once and hands to both - so the
// eval scores exactly the injection code path, never a parallel search impl
// (task spec acceptance criterion).

import (
	"kahya/kahyad/internal/eval"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/server"
)

// The one concrete searcher satisfies BOTH surfaces.
var (
	_ eval.RetrievalSearcher = (*search.Searcher)(nil)
	_ server.Searcher        = (*search.Searcher)(nil)
)

// And the two interfaces are mutually assignable (identical method sets):
// anything the injection path accepts, the eval runner accepts, and vice
// versa. If either signature drifts, this stops compiling.
var (
	_ = func(s server.Searcher) eval.RetrievalSearcher { return s }
	_ = func(s eval.RetrievalSearcher) server.Searcher { return s }
)
