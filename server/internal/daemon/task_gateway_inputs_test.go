package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTaskGatewayInputsUseOnlyClaimContent(test *testing.T) {
	task := Task{ID: "00000000-0000-4000-8000-000000000001", AgentID: "00000000-0000-4000-8000-000000000004", IssueID: "00000000-0000-4000-8000-000000000005", WorkspaceContext: "authorized workspace context", AuthToken: "mat_owned-task-secret", TaskGatewayDaemonToken: "mdt_owned-daemon-secret", RemoteMCPDaemonToken: "mdt_owned-mcp-secret"}
	task.Agent = &AgentData{ID: task.AgentID, Instructions: "authorized agent instructions", CustomEnv: map[string]string{"OPENAI_API_KEY": "owned-unlimited-secret"}, McpConfig: []byte(`{"secret":"owned-mcp-secret"}`), Skills: []SkillData{{Name: "owned", Content: "authorized skill", Files: []SkillFileData{{Path: "notes/proof.md", Content: "authorized supporting file"}}}}}
	task.Agent.Name = "Owned Native Agent"
	task.ProjectID, task.ProjectTitle, task.ProjectDescription = "00000000-0000-4000-8000-000000000006", "Owned Project", "authorized project context"
	task.ProjectResources = []ProjectResourceData{{ID: "owned-resource", ResourceType: "github_repo", ResourceRef: json.RawMessage(`{"url":"https://example.invalid/owned.git"}`), Label: "owned repository"}}
	for _, provider := range []string{"claude", "codex"} {
		inputs, err := taskGatewayInputs(task, provider)
		if err != nil {
			test.Fatal(err)
		}
		if string(inputs["multica-input/prompt.md"]) != BuildPrompt(task, provider) || string(inputs["multica-input/instructions.md"]) != task.Agent.Instructions || string(inputs["multica-input/workspace.md"]) != task.WorkspaceContext || string(inputs["multica-input/skills/owned/SKILL.md"]) != "authorized skill" || string(inputs["multica-input/skills/owned/notes/proof.md"]) != "authorized supporting file" || len(inputs) != 7 {
			test.Fatal("claim input selection changed")
		}
		for _, expected := range []string{task.Agent.Name, task.Agent.Instructions, task.WorkspaceContext, task.ProjectTitle, task.ProjectDescription, "owned repository", "owned", "Instruction Precedence"} {
			if !strings.Contains(string(inputs["multica-input/runtime.md"]), expected) {
				test.Fatal("runtime brief lost authorized task context", expected)
			}
		}
		var resources struct {
			ProjectID string                `json:"project_id"`
			Resources []ProjectResourceData `json:"resources"`
		}
		if json.Unmarshal(inputs["multica-input/project/resources.json"], &resources) != nil || resources.ProjectID != task.ProjectID || len(resources.Resources) != 1 || resources.Resources[0].ID != "owned-resource" {
			test.Fatal("authorized project sidecar unavailable")
		}
		for _, contents := range inputs {
			if strings.Contains(string(contents), "secret") {
				test.Fatal("staged task/daemon/custom/MCP credential")
			}
		}
	}
}

func TestTaskGatewayInputsRefuseUnresolvedAndForgedClaim(test *testing.T) {
	task := Task{AgentID: "00000000-0000-4000-8000-000000000004"}
	task.Agent = &AgentData{ID: task.AgentID}
	for _, kind := range []string{"missing-agent", "changed-agent", "skill-ref", "unsafe-skill", "unsafe-file", "duplicate-skill", "duplicate-file", "skill-entry-collision", "oversized", "oversized-brief", "invalid-project-json"} {
		test.Run(kind, func(test *testing.T) {
			candidate := task
			agent := *task.Agent
			candidate.Agent = &agent
			switch kind {
			case "missing-agent":
				candidate.Agent = nil
			case "changed-agent":
				candidate.Agent.ID = "forged"
			case "skill-ref":
				candidate.Agent.SkillRefs = []SkillRefData{{ID: "unresolved"}}
			case "unsafe-skill":
				candidate.Agent.Skills = []SkillData{{Name: "../escape"}}
			case "unsafe-file":
				candidate.Agent.Skills = []SkillData{{Name: "owned", Files: []SkillFileData{{Path: "../escape"}}}}
			case "duplicate-skill":
				candidate.Agent.Skills = []SkillData{{Name: "owned"}, {Name: "owned"}}
			case "duplicate-file":
				candidate.Agent.Skills = []SkillData{{Name: "owned", Files: []SkillFileData{{Path: "proof.md"}, {Path: "proof.md"}}}}
			case "skill-entry-collision":
				candidate.Agent.Skills = []SkillData{{Name: "owned", Files: []SkillFileData{{Path: "SKILL.md"}}}}
			case "oversized":
				candidate.Agent.Instructions = strings.Repeat("a", 1<<20+1)
			case "oversized-brief":
				candidate.Agent.Instructions = strings.Repeat("a", 1<<19)
				candidate.WorkspaceContext = strings.Repeat("b", 1<<19)
			case "invalid-project-json":
				candidate.ProjectResources = []ProjectResourceData{{ResourceRef: json.RawMessage(`{"invalid"`)}}
			}
			if inputs, err := taskGatewayInputs(candidate, "codex"); err == nil || inputs != nil {
				test.Fatal("unsafe/unresolved claim produced staged input")
			}
		})
	}
}
