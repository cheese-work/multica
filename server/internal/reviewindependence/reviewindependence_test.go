package reviewindependence

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

const valid = `{"attribute":"lab","rule":"reviewer_value_not_in_author_values",
 "agent_values":{"claude/claude-opus-4":"team-a","codex/*":"team-b"},
 "commit_identities":{"bot@a.example":"team-a","b.example":"team-b"}}`

func TestParseAcceptsTheDocumentedShape(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Attribute != "lab" || len(cfg.AgentValues) != 2 || len(cfg.CommitIdentities) != 2 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestParseRefusesBadShapes(t *testing.T) {
	cases := map[string]string{
		"not an object":      `[]`,
		"no attribute":       `{"rule":"reviewer_value_not_in_author_values"}`,
		"unknown rule":       `{"attribute":"lab","rule":"other"}`,
		"unknown field":      `{"attribute":"lab","rule":"reviewer_value_not_in_author_values","extra":1}`,
		"agent key no slash": `{"attribute":"lab","rule":"reviewer_value_not_in_author_values","agent_values":{"claude":"x"}}`,
		"agent empty value":  `{"attribute":"lab","rule":"reviewer_value_not_in_author_values","agent_values":{"claude/*":""}}`,
		"identity empty":     `{"attribute":"lab","rule":"reviewer_value_not_in_author_values","commit_identities":{"a.example":""}}`,
		"identity wildcard":  `{"attribute":"lab","rule":"reviewer_value_not_in_author_values","commit_identities":{"*.a.example":"x"}}`,
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestAgentValueIsExactThenProviderWildcardThenNone(t *testing.T) {
	cfg, _ := Parse([]byte(valid))
	for _, c := range []struct{ provider, model, want string }{
		{"claude", "claude-opus-4", "team-a"},
		{"CLAUDE", "Claude-Opus-4", "team-a"},
		{"claude", "claude-sonnet-9", ""},
		{"codex", "anything", "team-b"},
		{"gemini", "x", ""},
	} {
		if got := cfg.AgentValue(c.provider, c.model); got != c.want {
			t.Errorf("AgentValue(%q,%q) = %q, want %q", c.provider, c.model, got, c.want)
		}
	}
}

func TestCommitValuesReadAuthorCommitterAndTrailers(t *testing.T) {
	cfg, _ := Parse([]byte(valid))
	got := cfg.CommitValues(PRCommit{
		AuthorEmail: "BOT@a.example", CommitterEmail: "x@noreply.example",
		Message: "fix\n\nCo-authored-by: Codex <agent@b.example>\nCo-authored-by: Z <z@a.example>",
	})
	slices.Sort(got)
	if !slices.Equal(got, []string{"team-a", "team-b"}) {
		t.Fatalf("values = %v", got)
	}
}

func TestCommitValuesDomainIsExactNotSuffix(t *testing.T) {
	cfg, _ := Parse([]byte(valid))
	for _, email := range []string{"x@evilb.example", "x@b.example.evil.test", "x@sub.b.example", "b.example", ""} {
		if got := cfg.CommitValues(PRCommit{AuthorEmail: email}); len(got) != 0 {
			t.Errorf("%q matched %v", email, got)
		}
	}
}

func TestFromSettings(t *testing.T) {
	if _, ok := FromSettings(map[string]any{}); ok {
		t.Fatal("absent setting must be off")
	}
	if _, ok := FromSettings(map[string]any{SettingsKey: "junk"}); ok {
		t.Fatal("malformed setting must be off")
	}
	settings := map[string]any{SettingsKey: map[string]any{
		"attribute": "lab", "rule": Rule, "agent_values": map[string]any{"codex/*": "b"},
	}}
	cfg, ok := FromSettings(settings)
	if !ok || cfg.AgentValue("codex", "m") != "b" {
		t.Fatalf("cfg=%+v ok=%v", cfg, ok)
	}
}

func TestStoredCommitsKeepOnlyIdentitiesAndServeOnlyTheirHead(t *testing.T) {
	in := []PRCommit{{SHA: "abc", AuthorEmail: "a@x.example", CommitterEmail: "b@x.example", Message: "title\n\nbody text\nCo-authored-by: Bot <bot@y.example>"}}
	raw, err := json.Marshal(NewStoredCommits("h1", in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "body text") {
		t.Fatalf("stored column keeps message text: %s", raw)
	}
	got, ok := CommitsAt(raw, "h1")
	if !ok || len(got) != 1 {
		t.Fatalf("CommitsAt(h1) = %v, %v", got, ok)
	}
	cfg := Config{CommitIdentities: map[string]string{"y.example": "y"}}
	if v := cfg.CommitValues(got[0]); len(v) != 1 || v[0] != "y" {
		t.Fatalf("the stored trailer no longer maps: %v", v)
	}
	for name, c := range map[string]struct {
		raw  []byte
		head string
	}{"other head": {raw, "h2"}, "no head": {raw, ""}, "null": {[]byte("null"), "h1"}, "empty": {nil, "h1"}, "garbage": {[]byte("{"), "h1"}} {
		if _, ok := CommitsAt(c.raw, c.head); ok {
			t.Fatalf("%s: commits served", name)
		}
	}
}
