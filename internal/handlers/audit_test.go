package handlers

import "testing"

// The prefix filter is what lets one screen tell a feature's whole story
// (open, close, grant, revoke) instead of four partial ones. Its cleaning
// matters for one reason above the others: a list of blanks must produce NO
// filter, never a `LIKE '%'` that quietly returns the entire trail.
func TestAuditActionPrefixesCleansTheFilter(t *testing.T) {
	if got := auditActionPrefixes(nil); got != nil {
		t.Errorf("no filter must stay no filter, got %v", got)
	}

	blanks := []string{"", "   "}
	if got := auditActionPrefixes(&blanks); len(got) != 0 {
		t.Errorf("blank prefixes must not become a match-everything filter, got %v", got)
	}

	mixed := []string{" port-forward. ", "", "external-endpoint."}
	got := auditActionPrefixes(&mixed)
	if len(got) != 2 || got[0] != "port-forward." || got[1] != "external-endpoint." {
		t.Errorf("prefixes = %v, want the two trimmed non-empty ones", got)
	}

	long := make([]string, auditPrefixCap+5)
	for i := range long {
		long[i] = "a"
	}
	if got := auditActionPrefixes(&long); len(got) != auditPrefixCap {
		t.Errorf("prefix count = %d, want it capped at %d — each one is a LIKE", len(got), auditPrefixCap)
	}
}
