package markdownlinks

import "testing"

func destinations(refs []Reference) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Destination)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The three node kinds that render as a clickable destination are all in scope.
func TestFindCollectsEveryClickableNodeKind(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{name: "inline link", body: "see [the plan](https://example.com/plan.md)", want: []string{"https://example.com/plan.md"}},
		{name: "image", body: "![shot](https://example.com/a.png)", want: []string{"https://example.com/a.png"}},
		{name: "autolink", body: "<https://example.com/auto>", want: []string{"https://example.com/auto"}},
		{name: "reference link", body: "[plan][ref]\n\n[ref]: https://example.com/ref.md", want: []string{"https://example.com/ref.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := destinations(Find(tc.body))
			if !equalStrings(got, tc.want) {
				t.Errorf("Find(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// Code spans and fenced blocks are structurally invisible to the CommonMark
// parser's inline phase. This is the property that lets a comment DISCUSS a
// path or URL without the discussion counting as a link.
func TestFindIgnoresCodeConstructs(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "code span", body: "see `[the plan](https://example.com/plan.md)` for the shape"},
		{name: "fenced block", body: "```\n[the plan](https://example.com/plan.md)\n```"},
		{name: "indented block", body: "    [the plan](https://example.com/plan.md)"},
		{name: "bare path in code span", body: "reference it as `path/to/file.ts:42`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Find(tc.body); len(got) != 0 {
				t.Errorf("Find(%q) = %v, want no references", tc.body, destinations(got))
			}
		})
	}
}

// A link inside a blockquote IS emitted by the parser, so callers that must not
// accept quoted material need to tell the two apart. CHE-408 requires this:
// quoting someone else's link is not the same as declaring your own source.
func TestFindMarksQuotedReferences(t *testing.T) {
	body := "> they linked [theirs](https://example.com/quoted.md)\n\nand here is [mine](https://example.com/mine.md)"
	refs := Find(body)
	if len(refs) != 2 {
		t.Fatalf("Find returned %d references, want 2: %v", len(refs), destinations(refs))
	}
	if !refs[0].Quoted {
		t.Errorf("reference %q inside a blockquote must be marked Quoted", refs[0].Destination)
	}
	if refs[1].Quoted {
		t.Errorf("reference %q outside a blockquote must not be marked Quoted", refs[1].Destination)
	}
}

func TestFindMarksNestedBlockquoteAsQuoted(t *testing.T) {
	refs := Find("> > deeply [quoted](https://example.com/deep.md)")
	if len(refs) != 1 {
		t.Fatalf("Find returned %d references, want 1", len(refs))
	}
	if !refs[0].Quoted {
		t.Error("a reference nested two blockquotes deep must be marked Quoted")
	}
}

// A link inside a list inside a blockquote is still quoted: the walk must
// inspect every ancestor, not just the immediate parent.
func TestFindMarksQuotedThroughIntermediateContainer(t *testing.T) {
	refs := Find("> - item with [link](https://example.com/inlist.md)")
	if len(refs) != 1 {
		t.Fatalf("Find returned %d references, want 1", len(refs))
	}
	if !refs[0].Quoted {
		t.Error("a reference inside a list inside a blockquote must be marked Quoted")
	}
}

// De-duplication is by destination: one destination linked five times is one
// reference, so a caller reporting findings does not repeat itself.
func TestFindDeduplicatesByDestination(t *testing.T) {
	body := "[a](https://example.com/x) and [b](https://example.com/x) and [c](https://example.com/y)"
	got := destinations(Find(body))
	want := []string{"https://example.com/x", "https://example.com/y"}
	if !equalStrings(got, want) {
		t.Errorf("Find = %v, want %v", got, want)
	}
}

// A destination first seen quoted and later used unquoted must not stay marked
// Quoted — otherwise dedup order would decide whether a real link counts.
func TestFindPrefersUnquotedOccurrence(t *testing.T) {
	body := "> quoted [x](https://example.com/x)\n\nreal [x](https://example.com/x)"
	refs := Find(body)
	if len(refs) != 1 {
		t.Fatalf("Find returned %d references, want 1", len(refs))
	}
	if refs[0].Quoted {
		t.Error("a destination that also appears unquoted must not be marked Quoted")
	}
}

func TestFindEmptyBody(t *testing.T) {
	if got := Find(""); len(got) != 0 {
		t.Errorf("Find(\"\") = %v, want no references", destinations(got))
	}
}

// Link text is never inspected — only the destination. A body whose text says
// "plan" but links elsewhere must report the real destination.
func TestFindReportsDestinationNotText(t *testing.T) {
	refs := Find("[the authoritative 01-22 plan](https://example.com/unrelated.md)")
	if len(refs) != 1 || refs[0].Destination != "https://example.com/unrelated.md" {
		t.Fatalf("Find = %v, want the destination not the text", destinations(refs))
	}
}
