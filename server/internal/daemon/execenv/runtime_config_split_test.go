package execenv

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// splitFixture is one agent on one task; vary the task part with withTask.
func splitFixture(issueID, project string, repos []RepoContextForEnv, user string) TaskContextForEnv {
	return TaskContextForEnv{
		IssueID:                          issueID,
		AgentID:                          "agent-1",
		AgentName:                        "x99-claude-sonnet-200k",
		AgentInstructions:                "Implementation author.",
		WorkspaceContext:                 "Standing delivery authorization.",
		RequestingUserName:               user,
		RequestingUserProfileDescription: "PM, PO. Prefer TDD.",
		ProjectID:                        "proj-" + project,
		ProjectTitle:                     project,
		ProjectDescription:               "Review gates: " + project,
		ProjectResources: []ProjectResourceForEnv{
			{ID: "r-" + project, ResourceType: "github_repo", ResourceRef: json.RawMessage(`{"url":"https://example.com/` + project + `.git"}`)},
		},
		Repos:       repos,
		AgentSkills: []SkillContextForEnv{{Name: "tdd", Content: "x"}},
	}
}

var sectionStart = regexp.MustCompile(`(?m)^#{1,3} `)

// sections cuts a brief at every line that starts with "#", "##" or "###".
func sections(brief string) []string {
	idx := sectionStart.FindAllStringIndex(brief, -1)
	var out []string
	for i, m := range idx {
		end := len(brief)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		out = append(out, brief[m[0]:end])
	}
	return out
}

// CHE-6540 AC3: the stable part is byte-equal for two tasks of one agent on
// different issues, projects, repos and requesting users.
func TestSplitRuntimeBriefStableAcrossTasks(t *testing.T) {
	a := splitFixture("issue-a", "alpha", []RepoContextForEnv{{URL: "https://example.com/a.git", Ref: "main"}}, "Ann")
	b := splitFixture("issue-b", "beta", []RepoContextForEnv{{URL: "https://example.com/b.git"}}, "Bob")
	stableA, perTaskA := SplitRuntimeBrief("claude", a)
	stableB, perTaskB := SplitRuntimeBrief("claude", b)
	if stableA != stableB {
		t.Fatalf("stable brief differs between tasks of one agent:\nA:\n%s\nB:\n%s", stableA, stableB)
	}
	if perTaskA == perTaskB {
		t.Fatalf("per-task part must differ between tasks, got equal:\n%s", perTaskA)
	}
	for _, per := range []string{"alpha", "https://example.com/a.git", "PM, PO"} {
		if strings.Contains(stableA, per) {
			t.Errorf("stable brief holds per-task text %q", per)
		}
		if !strings.Contains(perTaskA, per) {
			t.Errorf("per-task part lost %q", per)
		}
	}
}

// CHE-6540 AC2: nothing is cut. Every section of the single-file brief is in
// the stable part or the per-task part, byte for byte.
func TestSplitRuntimeBriefKeepsEverySection(t *testing.T) {
	for _, repos := range [][]RepoContextForEnv{
		nil,
		{{URL: "https://example.com/a.git", Description: "app", Ref: "main"}},
	} {
		ctx := splitFixture("issue-a", "alpha", repos, "Ann")
		whole := RuntimeBrief("claude", ctx)
		stable, perTask := SplitRuntimeBrief("claude", ctx)
		for _, s := range sections(whole) {
			if !strings.Contains(stable, s) && !strings.Contains(perTask, s) {
				t.Errorf("section lost by the split:\n%s", s)
			}
		}
	}
}

// An issue-less task kind keeps the same promise: no section is dropped.
func TestSplitRuntimeBriefChatKindKeepsEverySection(t *testing.T) {
	ctx := splitFixture("", "alpha", nil, "Ann")
	ctx.IssueID = ""
	ctx.ChatSessionID = "chat-1"
	whole := RuntimeBrief("claude", ctx)
	stable, perTask := SplitRuntimeBrief("claude", ctx)
	for _, s := range sections(whole) {
		if !strings.Contains(stable, s) && !strings.Contains(perTask, s) {
			t.Errorf("section lost by the split:\n%s", s)
		}
	}
}
