package execenv

import (
	"strings"
	"testing"
)

// The fork routes a CLI gap to the workspace owner by comment. Upstream's
// wording ("report the gap in your result") has no owner and no comment, so
// taking upstream on this conflict point would silently drop the escalation.
func TestWriteAlwaysUseCLIKeepsForkOwnerEscalation(t *testing.T) {
	var b strings.Builder
	writeAlwaysUseCLI(&b)
	got := b.String()
	want := "post a comment mentioning the workspace owner"
	if !strings.Contains(got, want) {
		t.Fatalf("always-use-CLI section lacks %q:\n%s", want, got)
	}
	if strings.Contains(got, "report the gap in your result") {
		t.Fatalf("upstream wording leaked into the section:\n%s", got)
	}
}
