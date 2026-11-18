package browserlease

import "testing"

// W3-12 class rules (plan W1 r5, D3 = A): S0 user gate only; S2 decision per lease; H Cheese
// only, never leased to an agent; S1 and anything else rejected.
func TestClassRules(t *testing.T) {
	for _, tc := range []struct {
		class                 string
		mintable, needsDecide bool
	}{
		{ClassS0, true, false},
		{ClassS2, true, true},
		{ClassH, false, true},
		{"S1", false, true},
		{"", false, true},
		{"class3", false, true},
	} {
		if got := mintable(tc.class); got != tc.mintable {
			t.Errorf("mintable(%q) = %v, want %v", tc.class, got, tc.mintable)
		}
		if got := needsDecision(tc.class); got != tc.needsDecide {
			t.Errorf("needsDecision(%q) = %v, want %v", tc.class, got, tc.needsDecide)
		}
	}
}
