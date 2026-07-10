package policy

import "testing"

func TestCheckAllowsOnlyMemorySearch(t *testing.T) {
	d := Check("memory_search")
	if !d.Allow {
		t.Fatalf("Check(memory_search).Allow = false, want true")
	}
	if d.Reason != "" {
		t.Errorf("Check(memory_search).Reason = %q, want empty", d.Reason)
	}
	if d.Rule != RuleInterimStaticV1 {
		t.Errorf("Check(memory_search).Rule = %q, want %q", d.Rule, RuleInterimStaticV1)
	}
}

func TestCheckDeniesEveryOtherKnownTool(t *testing.T) {
	tools := []string{
		"memory_write", "memory_forget",
		"Read", "Glob", "Grep",
		"Bash", "WebFetch", "WebSearch", "Write", "Edit",
	}
	for _, tool := range tools {
		t.Run(tool, func(t *testing.T) {
			d := Check(tool)
			if d.Allow {
				t.Fatalf("Check(%s).Allow = true, want false", tool)
			}
			if d.Reason != reasonInterimDeny {
				t.Errorf("Check(%s).Reason = %q, want %q", tool, d.Reason, reasonInterimDeny)
			}
			if d.Rule != RuleInterimStaticV1 {
				t.Errorf("Check(%s).Rule = %q, want %q", tool, d.Rule, RuleInterimStaticV1)
			}
		})
	}
}

func TestCheckDeniesUnknownToolWithDistinctReason(t *testing.T) {
	d := Check("some_future_tool")
	if d.Allow {
		t.Fatalf("Check(unknown).Allow = true, want false")
	}
	if d.Reason != reasonUnknownTool {
		t.Errorf("Check(unknown).Reason = %q, want %q", d.Reason, reasonUnknownTool)
	}
	if d.Reason == reasonInterimDeny {
		t.Errorf("unknown tool must not reuse the known-deny reason")
	}
}

func TestCanonicalizeStripsSDKPrefix(t *testing.T) {
	cases := map[string]string{
		"mcp__kahya_memory__memory_write":  "memory_write",
		"mcp__kahya_memory__memory_search": "memory_search",
		"memory_search":                    "memory_search",
		"Read":                             "Read",
		"mcp__onlyoneunderscore":           "mcp__onlyoneunderscore",
		"mcp__":                            "mcp__",
	}
	for in, want := range cases {
		if got := Canonicalize(in); got != want {
			t.Errorf("Canonicalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckCanonicalizesSDKPrefixedNames(t *testing.T) {
	if d := Check("mcp__kahya_memory__memory_search"); !d.Allow {
		t.Fatalf("Check(mcp__kahya_memory__memory_search).Allow = false, want true (prefix must be canonicalized)")
	}
	d := Check("mcp__kahya_memory__memory_write")
	if d.Allow {
		t.Fatalf("Check(mcp__kahya_memory__memory_write).Allow = true, want false")
	}
	if d.Reason != reasonInterimDeny {
		t.Errorf("Reason = %q, want %q", d.Reason, reasonInterimDeny)
	}
}
