package decisionaction

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const repoCreateWorkspace = "11111111-1111-4111-8111-111111111111"

func repoCreateParams(override map[string]any) json.RawMessage {
	params := map[string]any{"workspace_id": repoCreateWorkspace, "org": "tevo-studio", "name": "we006-heylo-live", "visibility": "private", "template": "tevo-studio/code-android-template", "description": "WE006"}
	for key, value := range override {
		if value == nil {
			delete(params, key)
		} else {
			params[key] = value
		}
	}
	raw, _ := json.Marshal(params)
	return raw
}

func TestGitHubRepoCreateIsRegistered(test *testing.T) {
	definition, ok := Lookup(GitHubRepoCreate, 1)
	if !ok || definition.Passkey != PasskeyRequired || definition.Target != TargetWorkspace || !definition.OwnerOnly {
		test.Fatalf("definition = %+v", definition)
	}
}

func TestGitHubRepoCreateNormalizesAndIsStable(test *testing.T) {
	a, err := Normalize(Request{Type: GitHubRepoCreate, Version: 1, Params: repoCreateParams(map[string]any{"org": "Tevo-Studio"})})
	if err != nil {
		test.Fatal(err)
	}
	b, err := Normalize(Request{Type: GitHubRepoCreate, Version: 1, Params: repoCreateParams(nil)})
	if err != nil || string(a.Params) != string(b.Params) {
		test.Fatalf("org case changed the payload: %s vs %s (%v)", a.Params, b.Params, err)
	}
}

func TestGitHubRepoCreateRefusals(test *testing.T) {
	cases := map[string]struct {
		override map[string]any
		code     string
		slots    []SecretSlot
	}{
		"org not approved":      {map[string]any{"org": "evil-org"}, CodeOrgNotAllowed, nil},
		"template org":          {map[string]any{"template": "evil-org/tpl"}, CodeOrgNotAllowed, nil},
		"bad visibility":        {map[string]any{"visibility": "secret"}, CodeInvalidParams, nil},
		"missing visibility":    {map[string]any{"visibility": nil}, CodeInvalidParams, nil},
		"bad name":              {map[string]any{"name": "a/b"}, CodeInvalidParams, nil},
		"dot dot name":          {map[string]any{"name": ".."}, CodeInvalidParams, nil},
		"git suffix":            {map[string]any{"name": "x.git"}, CodeInvalidParams, nil},
		"bad template":          {map[string]any{"template": "tevo-studio"}, CodeInvalidParams, nil},
		"multiline description": {map[string]any{"description": "a\nb"}, CodeInvalidParams, nil},
		"long description":      {map[string]any{"description": strings.Repeat("a", 351)}, CodeInvalidParams, nil},
		"bad workspace id":      {map[string]any{"workspace_id": "nope"}, CodeInvalidParams, nil},
		"unknown field":         {map[string]any{"token": "x"}, CodeInvalidParams, nil},
		"secret slot":           {nil, CodeInvalidParams, []SecretSlot{{Slot: "t"}}},
	}
	for name, tc := range cases {
		test.Run(name, func(test *testing.T) {
			_, err := Normalize(Request{Type: GitHubRepoCreate, Version: 1, Params: repoCreateParams(tc.override), SecretSlots: tc.slots})
			var coded *Error
			if !errors.As(err, &coded) || coded.Code != tc.code {
				test.Fatalf("err = %v, want code %s", err, tc.code)
			}
		})
	}
}
