package decisiontx

import (
	"fmt"
	"strings"
	"testing"
)

func TestExcerpt(test *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"first nonblank line": {"\n\n## **Ship** it\nsecond", "Ship it"},
		"mentions and links":  {"> [@Sol](mention://agent/1) see [doc](https://x.io/a) `now`", "@Sol see doc now"},
		"curly quotes":        {"say “hi”", `say "hi"`},
		"only markup":         {"***\n", ""},
		"html tags":           {"<b>Important choice</b> <i>now</i>", "Important choice now"},
		"stray angle":         {"x > y", "x y"},
		"image":               {"![diagram](https://x.io/a.png) shown", "diagram shown"},
		"cap":                 {strings.Repeat("a", 101), strings.Repeat("a", 100) + "…"},
		"exactly 100":         {strings.Repeat("é", 100), strings.Repeat("é", 100)},
	} {
		if got := Excerpt(tc.in); got != tc.want {
			test.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

func TestWithExcerptsCapsAtFiveAndSkipsEmpty(test *testing.T) {
	ask := decodedAsk(test)
	var links []string
	for index := 0; index < 7; index++ {
		links = append(links, fmt.Sprintf("[c](/issue/i#comment-00000000-0000-0000-0000-00000000000%d)", index))
	}
	ask.Decision = strings.Join(links, " ")
	loaded := 0
	got, err := ask.WithExcerpts(func(id string) (string, error) {
		loaded++
		if strings.HasSuffix(id, "1") {
			return "", nil
		}
		return "text " + id[len(id)-1:], nil
	})
	if err != nil || loaded != 5 || strings.Count(got.Decision, " (“") != 4 {
		test.Fatalf("loaded=%d err=%v decision=%q", loaded, err, got.Decision)
	}
	if ask.Decision != strings.Join(links, " ") {
		test.Fatal("input ask mutated")
	}
}

func TestWithoutEnrichmentRestoresCallerText(test *testing.T) {
	ask := decodedAsk(test)
	base := ask.Content("ws")
	ask.AskedBy = "x99 agent"
	ask.Decision += " [c](/issue/i#comment-00000000-0000-0000-0000-000000000001)"
	plain := ask
	plain.AskedBy = ""
	enriched, _ := ask.WithExcerpts(func(string) (string, error) { return "hello", nil })
	if !strings.Contains(enriched.Content("ws"), "**Asked by** x99 agent\n\n**Agent did**") {
		test.Fatal("Asked by must be the first line of the Details block")
	}
	if WithoutEnrichment(enriched.Content("ws")) != WithoutEnrichment(plain.Content("ws")) {
		test.Fatal("enrichment leaked into identity")
	}
	if WithoutEnrichment(base) != WithoutEnrichment(decodedAsk(test).Content("ws")) {
		test.Fatal("unenriched identity drifted")
	}
}

func TestWithoutEnrichmentKeepsCallerQuotes(test *testing.T) {
	first, second := "Deploy (“canary”)?", "Deploy (“production”)?"
	if WithoutEnrichment(first) != first || WithoutEnrichment(first) == WithoutEnrichment(second) {
		test.Fatalf("caller quotes were stripped: %q", WithoutEnrichment(first))
	}
	link := "[c](/issue/i#comment-00000000-0000-0000-0000-000000000001)"
	if got := WithoutEnrichment(link + " (“hello”) and (“kept”)"); got != link+" and (“kept”)" {
		test.Fatalf("excerpt after a comment link must go, other quotes stay: %q", got)
	}
}
