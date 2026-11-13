package issueworkflow

import "testing"

const (
	designWF = "8394690d-632a-4b15-a333-c9f0247b0985"
	bugWF    = "11111111-1111-1111-1111-111111111111"
)

func TestWorkflowForTitle(t *testing.T) {
	settings := []byte(`{"workflow_by_type":{"design":"` + designWF + `","fix":"` + bugWF + `","bug":"` + bugWF + `","bad":"not-a-uuid"}}`)
	cases := []struct {
		name, title, want string
	}{
		{"scoped type", "design(x): new screen", designWF},
		{"unscoped type", "fix: crash on start", bugWF},
		{"breaking marker", "fix!(api): drop field", bugWF},
		{"case-insensitive", "Design(x): y", designWF},
		{"unmatched type", "feat(x): y", ""},
		{"no type", "just a title", ""},
		{"type not at start", "see design(x): y", ""},
		{"invalid id ignored", "bad(x): y", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, ok := WorkflowForTitle(settings, c.title)
			got := ""
			if ok {
				got = uuidString(id.Bytes)
			}
			if got != c.want {
				t.Fatalf("WorkflowForTitle(%q) = %q, want %q", c.title, got, c.want)
			}
		})
	}
	for _, s := range [][]byte{nil, []byte(`{}`), []byte(`not json`), []byte(`{"workflow_by_type":null}`)} {
		if _, ok := WorkflowForTitle(s, "design(x): y"); ok {
			t.Fatalf("settings %q matched", s)
		}
	}
}

func uuidString(b [16]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, x := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hex[x>>4], hex[x&15])
	}
	return string(out)
}

// CHE-5271: two keys that differ only by case must not pick at random.
func TestWorkflowForTitleDuplicateKeysAreDeterministic(t *testing.T) {
	settings := []byte(`{"workflow_by_type":{"Design":"00000000-0000-0000-0000-000000000002","design":"00000000-0000-0000-0000-000000000001"}}`)
	first, ok := WorkflowForTitle(settings, "design: x")
	if !ok {
		t.Fatal("no match")
	}
	for i := 0; i < 50; i++ {
		if got, _ := WorkflowForTitle(settings, "design: x"); got != first {
			t.Fatalf("run %d picked %v, first pick %v", i, got, first)
		}
	}
}
