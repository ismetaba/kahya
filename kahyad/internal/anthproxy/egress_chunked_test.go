package anthproxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/policy"
)

type chunkedTestBudget struct {
	mu sync.Mutex
	m  map[string]int64
}

func (b *chunkedTestBudget) Bytes(_ context.Context, host, day string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m[host+"|"+day], nil
}

func (b *chunkedTestBudget) Add(_ context.Context, host, day string, n int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[host+"|"+day] += n
	return b.m[host+"|"+day], nil
}

// TestProxyChunkedModelCallIsMeteredAgainstEgressBudget (FIX-FINAL
// egress-budget-chunked-bypass): through the real Proxy and the real
// W3-05 hook, a chunked /v1/messages body (no Content-Length) larger than
// the daily byte budget is refused with a 403 and never reaches the
// upstream - exactly like the same body sent with a Content-Length.
func TestProxyChunkedModelCallIsMeteredAgainstEgressBudget(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		upstream := newStatusSequenceUpstream(t, []int{200, 200, 200})
		ledger := &fakeLedger{}
		cfg := retryCfg(t, upstream.srv.URL, generousGovernor(), ledger, &fakeSleeper{})
		gate := egress.NewGate(policy.EgressConfig{
			Allowlist:              []policy.EgressAllowEntry{{Host: "api.anthropic.com"}},
			DefaultDailyByteBudget: 1000,
		}, egress.NewSensitiveTracker(), &chunkedTestBudget{m: map[string]int64{}}, nil, nil, nil)
		cfg.EgressGate = egress.NewAnthproxyEgressGateHook(gate, "api.anthropic.com", 443)("t1", "tr1")
		baseURL, _ := startTestProxy(t, cfg)
		body := `{"model":"claude-sonnet-5-5","pad":"` + strings.Repeat("x", 5000) + `"}`
		for i := 0; i < 2; i++ {
			var rdr io.Reader = strings.NewReader(body)
			if chunked {
				rdr = io.MultiReader(strings.NewReader(body)) // unknown length -> Transfer-Encoding: chunked
			}
			req, err := http.NewRequest("POST", baseURL+"/v1/messages", rdr)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(TaskTokenHeader, testToken)
			req.Header.Set("Authorization", "Bearer test-upstream-credential")
			if status, _ := doAndDrain(t, req); status != http.StatusForbidden {
				t.Fatalf("chunked=%v #%d: status=%d, want 403 (over the daily byte budget)", chunked, i, status)
			}
		}
		if got := upstream.hits(); got != 0 {
			t.Fatalf("chunked=%v: upstream hits = %d, want 0", chunked, got)
		}
	}
}
