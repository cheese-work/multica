package governance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const workspaceID = "11111111-1111-1111-1111-111111111111"
const projectID = "22222222-2222-2222-2222-222222222222"
const squadID = "33333333-3333-3333-3333-333333333333"
const agentID = "44444444-4444-4444-4444-444444444444"

func testRule(id, kind, scopeID string) Rule {
	return Rule{ID: id, Scope: RuleScope{Kind: kind, ID: scopeID}, Text: id, Check: "missing_mention_v1", Mode: "shadow", AllowedCorrections: []string{"mention_agent", "mention_squad"}}
}

func testRevision(t *testing.T, rules ...Rule) RuleRevision {
	t.Helper()
	data, err := json.Marshal(RuleRevision{SchemaVersion: 1, Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := ParseRuleRevision(data)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestRuleInheritanceAndOverrides(t *testing.T) {
	workspace := testRule("workspace", "workspace", workspaceID)
	workspace.OverridableBy = []RuleOverrideGrant{{Scope: "project", Fields: []string{"text", "allowed_corrections"}}}
	project := testRule("project", "project", projectID)
	project.Replaces = "workspace"
	project.AllowedCorrections = []string{"mention_agent"}
	squad := testRule("squad", "squad", squadID)
	agent := testRule("agent", "agent", agentID)
	context := RuleContext{WorkspaceID: workspaceID, ProjectID: projectID, SquadIDs: []string{squadID}, AgentID: agentID}
	result := ResolveRules(testRevision(t, workspace, project, squad, agent), context)
	if len(result.Effective) != 3 || len(result.Replaced) != 1 || len(result.Inherited) != 0 || len(result.Conflicting) != 0 {
		t.Fatalf("unexpected policy resolution: %+v", result)
	}
	if result.Effective[0].ID != "agent" || result.Effective[1].ID != "project" || result.Effective[2].ID != "squad" || result.Digest == "" {
		t.Fatalf("unstable resolution: %+v", result)
	}
	withoutProject := ResolveRules(testRevision(t, workspace, project, squad, agent), RuleContext{WorkspaceID: workspaceID, SquadIDs: []string{squadID}})
	if len(withoutProject.Inherited) != 1 || len(withoutProject.Effective) != 2 {
		t.Fatalf("workspace inheritance lost: %+v", withoutProject)
	}
}

func TestRuleConflictsAndNonExpandingGrants(t *testing.T) {
	workspace := testRule("workspace", "workspace", workspaceID)
	workspace.OverridableBy = []RuleOverrideGrant{{Scope: "project", Fields: []string{"text", "allowed_corrections"}}, {Scope: "squad", Fields: []string{"text", "allowed_corrections"}}}
	project := testRule("project", "project", projectID)
	project.Replaces = "workspace"
	squad := testRule("squad", "squad", squadID)
	squad.Replaces = "workspace"
	context := RuleContext{WorkspaceID: workspaceID, ProjectID: projectID, SquadIDs: []string{squadID}}
	result := ResolveRules(testRevision(t, workspace, project, squad), context)
	if len(result.Effective) != 0 || len(result.Conflicting) != 3 {
		t.Fatalf("competing replacements did not abstain: %+v", result)
	}
	squad.Replaces = ""
	squad.AllowedCorrections = []string{"mention_squad"}
	project.AllowedCorrections = []string{"mention_agent"}
	result = ResolveRules(testRevision(t, workspace, project, squad), context)
	if len(result.Conflicting) != 2 || len(result.Replaced) != 1 {
		t.Fatalf("contradictory corrections not exposed: %+v", result)
	}
	project.AllowedCorrections = []string{"assign_agent"}
	result = ResolveRules(testRevision(t, workspace, project), context)
	if len(result.Conflicting) != 1 || len(result.Effective) != 1 || result.Effective[0].ID != "workspace" {
		t.Fatalf("expanded action accepted: %+v", result)
	}
	project.AllowedCorrections = workspace.AllowedCorrections
	project.Mode = "correct"
	result = ResolveRules(testRevision(t, workspace, project), context)
	if len(result.Conflicting) != 1 {
		t.Fatalf("ungranted mode override accepted: %+v", result)
	}
	project.Replaces = ""
	project.Mode = "shadow"
	project.AllowedCorrections = []string{"assign_agent"}
	result = ResolveRules(testRevision(t, workspace, project), context)
	if len(result.Conflicting) != 1 || len(result.Effective) != 1 {
		t.Fatalf("additive scope expanded action grant: %+v", result)
	}
}

func TestRuleReplacementCannotDelegateBeyondAncestor(t *testing.T) {
	workspace := testRule("workspace", "workspace", workspaceID)
	workspace.OverridableBy = []RuleOverrideGrant{{Scope: "project", Fields: []string{"text"}}}
	project := testRule("project", "project", projectID)
	project.Replaces = workspace.ID
	project.OverridableBy = []RuleOverrideGrant{{Scope: "agent", Fields: []string{"text", "mode"}}}
	agent := testRule("agent", "agent", agentID)
	agent.Replaces = project.ID
	agent.Mode = "correct"
	result := ResolveRules(testRevision(t, workspace, project, agent), RuleContext{WorkspaceID: workspaceID, ProjectID: projectID, AgentID: agentID})
	if len(result.Effective) != 1 || result.Effective[0].ID != workspace.ID || len(result.Conflicting) != 2 {
		t.Fatalf("ungranted descendant override accepted: %+v", result)
	}
}

func TestRuleReplacementRetainsExplicitDelegation(t *testing.T) {
	workspace := testRule("workspace", "workspace", workspaceID)
	workspace.OverridableBy = []RuleOverrideGrant{{Scope: "project", Fields: []string{"text"}}, {Scope: "agent", Fields: []string{"text"}}}
	project := testRule("project", "project", projectID)
	project.Replaces = workspace.ID
	project.OverridableBy = []RuleOverrideGrant{{Scope: "agent", Fields: []string{"text"}}}
	agent := testRule("agent", "agent", agentID)
	agent.Replaces = project.ID
	result := ResolveRules(testRevision(t, workspace, project, agent), RuleContext{WorkspaceID: workspaceID, ProjectID: projectID, AgentID: agentID})
	if len(result.Effective) != 1 || result.Effective[0].ID != agent.ID || len(result.Replaced) != 2 || len(result.Conflicting) != 0 {
		t.Fatalf("authorized descendant override lost: %+v", result)
	}
}

func TestRuleConflictPropagatesToDescendants(t *testing.T) {
	workspace := testRule("workspace", "workspace", workspaceID)
	workspace.OverridableBy = []RuleOverrideGrant{
		{Scope: "project", Fields: []string{"text"}},
		{Scope: "squad", Fields: []string{"text"}},
		{Scope: "agent", Fields: []string{"text"}},
	}
	project := testRule("project", "project", projectID)
	project.Replaces, project.OverridableBy = workspace.ID, workspace.OverridableBy
	squad := testRule("squad", "squad", squadID)
	squad.Replaces, squad.OverridableBy = workspace.ID, workspace.OverridableBy
	agent := testRule("agent", "agent", agentID)
	agent.Replaces = project.ID
	result := ResolveRules(testRevision(t, workspace, project, squad, agent), RuleContext{WorkspaceID: workspaceID, ProjectID: projectID, SquadIDs: []string{squadID}, AgentID: agentID})
	if len(result.Effective) != 0 || len(result.Conflicting) != 4 {
		t.Fatalf("conflicted ancestor delegated authority: %+v", result)
	}
}

func TestRuleConflictingActionDoesNotSuppressValidReplacement(t *testing.T) {
	workspace := testRule("workspace", "workspace", workspaceID)
	workspace.OverridableBy = []RuleOverrideGrant{{Scope: "project", Fields: []string{"text"}}, {Scope: "squad", Fields: []string{"text"}}}
	project := testRule("project", "project", projectID)
	project.Replaces = workspace.ID
	squad := testRule("squad", "squad", squadID)
	squad.Replaces = workspace.ID
	squad.AllowedCorrections = []string{"assign_agent"}
	result := ResolveRules(testRevision(t, workspace, project, squad), RuleContext{WorkspaceID: workspaceID, ProjectID: projectID, SquadIDs: []string{squadID}})
	if len(result.Effective) != 1 || result.Effective[0].ID != project.ID || len(result.Conflicting) != 1 {
		t.Fatalf("invalid action contested valid replacement: %+v", result)
	}
}

func TestRuleManifestValidationAndCanonicalSource(t *testing.T) {
	rule := testRule("workspace", "workspace", workspaceID)
	revision := testRevision(t, rule)
	data, _ := json.Marshal(RuleRevision{SchemaVersion: 1, Rules: []Rule{rule}})
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "rules"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "rules", "manifest.json")
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRuleRevision(root, RuleRepository, "rules/manifest.json")
	if err != nil || loaded.Digest != revision.Digest {
		t.Fatalf("canonical load: %+v, %v", loaded, err)
	}
	if loaded.SourceDigest == "" || loaded.SourceDigest != revision.SourceDigest {
		t.Fatalf("source digest changed during load: %+v", loaded)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated.md"), []byte("unrelated changes"), 0600); err != nil {
		t.Fatal(err)
	}
	afterDocsEdit, err := LoadRuleRevision(root, RuleRepository, "rules/manifest.json")
	if err != nil || afterDocsEdit.Digest != loaded.Digest {
		t.Fatalf("unrelated document activated policy: %v", err)
	}
	for _, name := range []string{"docs/unrelated.json", "rules/../docs/secrets.json", "/rules/manifest.json", "rules/manifest.yaml"} {
		if _, err := LoadRuleRevision(root, RuleRepository, name); err == nil {
			t.Fatalf("unsafe path accepted: %q", name)
		}
	}
	if _, err := LoadRuleRevision(root, "another/repo", "rules/manifest.json"); err == nil {
		t.Fatal("wrong repository accepted")
	}
	if err := os.Symlink(manifest, filepath.Join(root, "rules", "link.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuleRevision(root, RuleRepository, "rules/link.json"); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.WriteFile(manifest, []byte(strings.Repeat(" ", maxRuleManifestBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuleRevision(root, RuleRepository, "rules/manifest.json"); err == nil {
		t.Fatal("oversize manifest accepted")
	}
	for name, input := range map[string]string{
		"unsupported":        `{"schema_version":2,"rules":[]}`,
		"unknown field":      `{"schema_version":1,"rules":[],"include":"docs/x"}`,
		"unknown action":     strings.Replace(string(data), "mention_agent", "wipe_workspace", 1),
		"unknown scope":      strings.Replace(string(data), `"kind":"workspace"`, `"kind":"unbounded"`, 1),
		"secret":             strings.Replace(string(data), "workspace", "api_key=12345678901234567890", 1),
		"escaped secret":     strings.Replace(string(data), `"text":"workspace"`, `"text":"\u0061pi_key=12345678901234567890"`, 1),
		"multiple documents": string(data) + "{}",
	} {
		if _, err := ParseRuleRevision([]byte(input)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestReferenceDigestAndUnsupportedCheck(t *testing.T) {
	rule := testRule("workspace", "workspace", workspaceID)
	revision := RuleRevision{SchemaVersion: 1, Rules: []Rule{rule}, References: []RuleReference{{Repository: RuleRepository, Commit: strings.Repeat("1", 40), Path: "standards/reference.md", SHA256: strings.Repeat("a", 64)}}}
	encode := func() []byte { data, _ := json.Marshal(revision); return data }
	first, err := ParseRuleRevision(encode())
	if err != nil {
		t.Fatal(err)
	}
	revision.References[0].SHA256 = strings.Repeat("b", 64)
	second, err := ParseRuleRevision(encode())
	if err != nil || first.Digest == second.Digest {
		t.Fatalf("reference digest ignored: %v", err)
	}
	revision.References[0].Path = "secrets/credentials.env"
	if _, err := ParseRuleRevision(encode()); err == nil {
		t.Fatal("secret reference accepted")
	}
	rule.Check = "unsupported_future_check"
	result := ResolveRules(testRevision(t, rule), RuleContext{WorkspaceID: workspaceID})
	if len(result.Effective) != 1 || len(result.Effective[0].AllowedCorrections) != 0 {
		t.Fatalf("unsupported semantic rule granted actions: %+v", result)
	}
	base := testRevision(t, testRule("workspace", "workspace", workspaceID))
	resolved := ResolveRules(base, RuleContext{WorkspaceID: workspaceID})
	resolved.Effective[0].AllowedCorrections[0] = "assign_agent"
	if base.Rules[0].AllowedCorrections[0] != "mention_agent" || resolved.Inherited[0].AllowedCorrections[0] != "mention_agent" {
		t.Fatal("mutable effective rule leaked into revision or inherited view")
	}
}
