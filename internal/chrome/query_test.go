package chrome

import "testing"

func TestNameMatches(t *testing.T) {
	cases := []struct {
		actual, want, mode string
		ok                 bool
	}{
		// exact (default)
		{"Review", "Review", "", true},
		{"Review", "Review", "exact", true},
		{"  Review  ", "Review", "exact", true}, // trimmed
		{"Review Approval: Awaiting Action", "Review", "exact", false},
		// contains — case-insensitive substring (the verbose-name case)
		{"Review Approval: Awaiting Action by Sanket", "Review", "contains", true},
		{"Go to My Tasks (2)", "my tasks", "contains", true},
		{"Approve", "Deny", "contains", false},
		// regex
		{"Review Approval 42", `Review.*\d+`, "regex", true},
		{"Review Approval", `^Approve`, "regex", false},
		{"anything", `(`, "regex", false}, // bad regex -> no match, no panic
	}
	for _, c := range cases {
		if got := nameMatches(c.actual, c.want, c.mode); got != c.ok {
			t.Errorf("nameMatches(%q,%q,%q) = %v, want %v", c.actual, c.want, c.mode, got, c.ok)
		}
	}
}
