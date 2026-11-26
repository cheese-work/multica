package capabilityrouting

import (
	"strings"
	"testing"
)

const (
	authorID    = "11111111-1111-4111-8111-111111111111"
	reviewerID  = "22222222-2222-4222-8222-222222222222"
	reviewerTwo = "33333333-3333-4333-8333-333333333333"
)

func TestValidateAcceptsATypeToAuthorAndReviewers(t *testing.T) {
	raw := `{"feat":{"author":"` + authorID + `","reviewers":["` + reviewerID + `","` + reviewerTwo + `"]},"fix":{"author":"` + authorID + `","reviewers":["` + reviewerID + `"]}}`
	if err := Validate([]byte(raw)); err != nil {
		t.Fatalf("valid table refused: %v", err)
	}
	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg["feat"]; got.Author != authorID || len(got.Reviewers) != 2 || got.Reviewers[0] != reviewerID {
		t.Fatalf("feat = %+v", got)
	}
	if ids := cfg.AgentIDs(); len(ids) != 3 || ids[0] != authorID {
		t.Fatalf("AgentIDs = %v, want 3 sorted ids", ids)
	}
}

func TestValidateRefusesBadTables(t *testing.T) {
	for name, tc := range map[string]struct{ raw, want string }{
		"not an object":       {`"feat"`, "object"},
		"null":                {`null`, "object"},
		"empty table":         {`{}`, "at least one"},
		"entry not an object": {`{"feat":"x"}`, "feat"},
		"empty reviewers":     {`{"feat":{"author":"` + authorID + `","reviewers":[]}}`, "reviewers"},
		"missing reviewers":   {`{"feat":{"author":"` + authorID + `"}}`, "reviewers"},
		"missing author":      {`{"feat":{"reviewers":["` + reviewerID + `"]}}`, "author"},
		"author not a uuid":   {`{"feat":{"author":"sonnet","reviewers":["` + reviewerID + `"]}}`, "author"},
		"reviewer not a uuid": {`{"feat":{"author":"` + authorID + `","reviewers":["terra"]}}`, "reviewers"},
		"author reviews":      {`{"feat":{"author":"` + authorID + `","reviewers":["` + authorID + `"]}}`, "author"},
		"duplicate reviewer":  {`{"feat":{"author":"` + authorID + `","reviewers":["` + reviewerID + `","` + reviewerID + `"]}}`, "twice"},
		"unknown field":       {`{"feat":{"author":"` + authorID + `","reviewers":["` + reviewerID + `"],"extra":1}}`, "extra"},
		"uppercase type":      {`{"Feat":{"author":"` + authorID + `","reviewers":["` + reviewerID + `"]}}`, "lowercase"},
		"space in type":       {`{"my type":{"author":"` + authorID + `","reviewers":["` + reviewerID + `"]}}`, "lowercase"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Validate([]byte(tc.raw))
			if err == nil {
				t.Fatalf("%s accepted", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseCanonicalizesUUIDForms(t *testing.T) {
	undashed := strings.ReplaceAll(strings.ToUpper(authorID), "-", "")
	for name, raw := range map[string]string{
		"undashed author as reviewer":  `{"feat":{"author":"` + authorID + `","reviewers":["` + undashed + `"]}}`,
		"uppercase author as reviewer": `{"feat":{"author":"` + authorID + `","reviewers":["` + strings.ToUpper(authorID) + `"]}}`,
		"dashed and undashed twice":    `{"feat":{"author":"` + authorID + `","reviewers":["` + reviewerID + `","` + strings.ReplaceAll(reviewerID, "-", "") + `"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatalf("%s accepted", raw)
			}
		})
	}
	cfg, err := Parse([]byte(`{"feat":{"author":"` + undashed + `","reviewers":["` + strings.ReplaceAll(reviewerID, "-", "") + `"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg["feat"]; got.Author != authorID || got.Reviewers[0] != reviewerID {
		t.Fatalf("ids not canonical: %+v", got)
	}
}

func TestFromSettingsReadsLeniently(t *testing.T) {
	settings := map[string]any{SettingsKey: map[string]any{
		"feat": map[string]any{"author": authorID, "reviewers": []any{reviewerID}},
		"bad":  "skipped",
	}}
	cfg := FromSettings(settings)
	if len(cfg) != 1 || cfg["feat"].Author != authorID {
		t.Fatalf("cfg = %+v, want only feat", cfg)
	}
	for _, empty := range []map[string]any{nil, {}, {SettingsKey: "x"}, {SettingsKey: nil}} {
		if got := FromSettings(empty); len(got) != 0 {
			t.Fatalf("FromSettings(%v) = %+v, want empty", empty, got)
		}
	}
}

// CHE-9318: a type the table does not list, or no type, takes the default entry.
func TestConfigForFallsBackToTheDefaultEntry(t *testing.T) {
	cfg := Config{"feat": {Author: "a"}, DefaultKey: {Author: "d"}}
	for typ, want := range map[string]string{"feat": "a", "task": "d", "": "d"} {
		if got, ok := cfg.For(typ); !ok || got.Author != want {
			t.Errorf("For(%q) = %q, %v; want %q", typ, got.Author, ok, want)
		}
	}
	if _, ok := (Config{"feat": {Author: "a"}}).For("task"); ok {
		t.Error("For(task) with no default entry routed")
	}
}
