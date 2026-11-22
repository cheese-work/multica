package mergequeue

import (
	"strings"
	"testing"
)

func TestValidateSetting(t *testing.T) {
	type obj = map[string]any
	for _, tc := range []struct {
		name    string
		in      any
		wantErr string // "" = valid
	}{
		{"not an object is not this validator's business", "x", ""},
		{"no keys", obj{}, ""},
		{"routing off", obj{RoutingKey: "off"}, ""},
		{"routing warn", obj{RoutingKey: "warn"}, ""},
		{"routing block is not available yet", obj{RoutingKey: "block"}, "routing-not-available"},
		{"routing outside the modes", obj{RoutingKey: "bogus"}, RoutingKey},
		{"routing empty", obj{RoutingKey: ""}, RoutingKey},
		{"routing null", obj{RoutingKey: nil}, RoutingKey},
		{"routing not a string", obj{RoutingKey: true}, RoutingKey},
		{"bindings empty object", obj{ActorBindingsKey: obj{}}, ""},
		{"bindings disjoint", obj{ActorBindingsKey: obj{"agents": []any{"congvc-bot-x99"}, "people": []any{"congvc-dev"}}}, ""},
		{"bindings login in both lists", obj{ActorBindingsKey: obj{"agents": []any{"a", "Shared"}, "people": []any{"shared"}}}, "both"},
		{"bindings not an object", obj{ActorBindingsKey: []any{"a"}}, ActorBindingsKey},
		{"bindings unknown list", obj{ActorBindingsKey: obj{"bots": []any{"a"}}}, ActorBindingsKey},
		{"bindings list not an array", obj{ActorBindingsKey: obj{"agents": "a"}}, ActorBindingsKey},
		{"bindings entry not a string", obj{ActorBindingsKey: obj{"agents": []any{1}}}, ActorBindingsKey},
		{"bindings empty login", obj{ActorBindingsKey: obj{"people": []any{" "}}}, ActorBindingsKey},
		{"watch issue empty", obj{WatchIssueKey: ""}, ""},
		{"watch issue uuid", obj{WatchIssueKey: "00000000-0000-4000-8000-000000000001"}, ""}, // gitleaks:allow test uuid
		{"watch issue not a uuid", obj{WatchIssueKey: "CHE-7408"}, WatchIssueKey},
		{"watch issue not a string", obj{WatchIssueKey: 7}, WatchIssueKey},
		{"one bad key among good ones", obj{RoutingKey: "warn", WatchIssueKey: "nope"}, WatchIssueKey},
	} {
		err := ValidateSetting(tc.in)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error = %v, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}
