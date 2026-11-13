package decisionaction_test

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	_ "github.com/multica-ai/multica/server/internal/handler" // registers the workspace.* action types
)

// The agent docs carry one worked --action-file example per config-change
// type. The docs are the fixtures: every json block with a "type" is parsed
// from decisions.md, so an example cannot drift from the registry.
const decisionsDoc = "../service/builtin_skills/multica-platform/references/decisions.md"

var jsonBlock = regexp.MustCompile("(?s)```json\n(.*?)```")

type docAction struct {
	Type    string          `json:"type"`
	Version int             `json:"version"`
	Params  json.RawMessage `json:"params"`
}

func docActions(t *testing.T) map[string]docAction {
	t.Helper()
	raw, err := os.ReadFile(decisionsDoc)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]docAction{}
	for _, match := range jsonBlock.FindAllStringSubmatch(string(raw), -1) {
		var action docAction
		decoder := json.NewDecoder(strings.NewReader(match[1]))
		decoder.DisallowUnknownFields() // the CLI envelope takes only type, version, params
		if decoder.Decode(&action) != nil || action.Type == "" {
			continue // not an action file (an ask file, a params snippet)
		}
		out[action.Type] = action
	}
	return out
}

func TestDocumentedActionExamplesNormalize(t *testing.T) {
	want := []string{
		"workspace.settings.patch", "agent.instructions.set", "workspace.context.set",
		"workspace.routing_agent.set", "workspace.delegated_writes.set", "agent.skills.set",
	}
	examples := docActions(t)
	for _, typ := range want {
		action, ok := examples[typ]
		if !ok {
			t.Errorf("decisions.md has no --action-file example for %s", typ)
			continue
		}
		if _, err := decisionaction.Normalize(decisionaction.Request{Type: action.Type, Version: action.Version, Params: action.Params}); err != nil {
			t.Errorf("%s example is refused: %v", typ, err)
		}
	}
}

// The text and settings examples must carry digests that are right for the
// values they show, so a reader who copies the recipe gets the same hex.
func TestDocumentedActionExampleDigests(t *testing.T) {
	examples := docActions(t)

	var p map[string]string
	if action, ok := examples["workspace.context.set"]; ok && json.Unmarshal(action.Params, &p) == nil {
		if got := decisionaction.TextDigest(p["context"]); got != p["after_sha256"] {
			t.Errorf("workspace.context.set after_sha256 = %s, digest of its context is %s", p["after_sha256"], got)
		}
	}
	if action, ok := examples["agent.instructions.set"]; ok && json.Unmarshal(action.Params, &p) == nil {
		if got := decisionaction.TextDigest(p["instructions"]); got != p["after_sha256"] {
			t.Errorf("agent.instructions.set after_sha256 = %s, digest of its text is %s", p["after_sha256"], got)
		}
	}
	for typ, key := range map[string]string{
		"workspace.routing_agent.set":    "routing_agent",
		"workspace.delegated_writes.set": "delegated_governed_writes",
	} {
		action, ok := examples[typ]
		if !ok {
			continue
		}
		var params map[string]json.RawMessage
		if err := json.Unmarshal(action.Params, &params); err != nil {
			t.Fatal(err)
		}
		var workspaceID, after string
		_ = json.Unmarshal(params["workspace_id"], &workspaceID)
		_ = json.Unmarshal(params["after_sha256"], &after)
		got, err := decisionaction.SettingsDigest(workspaceID, map[string]json.RawMessage{key: params[key]})
		if err != nil || got != after {
			t.Errorf("%s after_sha256 = %s, digest of its %s is %s (%v)", typ, after, key, got, err)
		}
	}
	if action, ok := examples["workspace.settings.patch"]; ok {
		var params struct {
			WorkspaceID string                     `json:"workspace_id"`
			Settings    map[string]json.RawMessage `json:"settings"`
			AfterSHA256 string                     `json:"after_sha256"`
		}
		if err := json.Unmarshal(action.Params, &params); err != nil {
			t.Fatal(err)
		}
		got, err := decisionaction.SettingsDigest(params.WorkspaceID, params.Settings)
		if err != nil || got != params.AfterSHA256 {
			t.Errorf("workspace.settings.patch after_sha256 = %s, digest of its settings is %s (%v)", params.AfterSHA256, got, err)
		}
	}
}

func TestDecisionsDocNamesDecisionPropose(t *testing.T) {
	raw, err := os.ReadFile(decisionsDoc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "multica decision propose") {
		t.Error("decisions.md does not name multica decision propose")
	}
}
