package service

import (
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/internal/reviewindependence"
)

const reviewValuesSetting = `{"review_independence":{"attribute":"team","rule":"reviewer_value_not_in_author_values",
 "agent_values":{"claude/*":"a","codex/*":"b"},
 "commit_identities":{"a.example":"a","b.example":"b"}}}`

func TestReviewValuesAbsentSettingKeepsLabOf(t *testing.T) {
	rv := ReviewValuesFromSettings([]byte(`{}`))
	if rv.Configured() || rv.Attribute() != "lab" {
		t.Fatalf("configured=%v attribute=%q", rv.Configured(), rv.Attribute())
	}
	if got := rv.AgentValue("claude", ""); got != LabOf("", "claude") || got == "" {
		t.Fatalf("AgentValue = %q, want LabOf's", got)
	}
}

func TestReviewValuesConfiguredSettingReplacesLabOf(t *testing.T) {
	rv := ReviewValuesFromSettings([]byte(reviewValuesSetting))
	if !rv.Configured() || rv.Attribute() != "team" {
		t.Fatalf("configured=%v attribute=%q", rv.Configured(), rv.Attribute())
	}
	if got := rv.AgentValue("claude", "x"); got != "a" {
		t.Fatalf("claude = %q", got)
	}
	if got := rv.AgentValue("gemini", "x"); got != "" {
		t.Fatalf("an unlisted agent has no value (fail closed), got %q", got)
	}
}

func TestReviewValuesCommitAuthorValues(t *testing.T) {
	rv := ReviewValuesFromSettings([]byte(reviewValuesSetting))
	a := reviewindependence.PRCommit{SHA: "fe81d960b4a5c0ffee", AuthorEmail: "x@a.example"}
	b := reviewindependence.PRCommit{SHA: "0123456789abcdef", Message: "m\n\nCo-authored-by: C <c@b.example>"}

	values, ok := rv.CommitAuthorValues([]reviewindependence.PRCommit{a, b})
	if !ok || !slices.Equal(values["a"], []string{"commit fe81d960b4a5"}) || !slices.Equal(values["b"], []string{"commit 0123456789ab"}) {
		t.Fatalf("values=%v ok=%v", values, ok)
	}
	for name, commits := range map[string][]reviewindependence.PRCommit{
		"no commits":       nil,
		"unmapped commit":  {a, {SHA: "aaaaaaaaaaaa", AuthorEmail: "x@evila.example"}},
		"look-alike (suf)": {{SHA: "bbbbbbbbbbbb", AuthorEmail: "x@sub.a.example"}},
	} {
		if v, ok := rv.CommitAuthorValues(commits); ok {
			t.Errorf("%s: want fall back to runs, got %v", name, v)
		}
	}
	if _, ok := ReviewValuesFromSettings([]byte(`{}`)).CommitAuthorValues([]reviewindependence.PRCommit{a}); ok {
		t.Error("with the setting absent commits must not decide")
	}
}
