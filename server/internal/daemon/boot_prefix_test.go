package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// armRoots holds one root key per arm, found by search so the test does not
// depend on the hash of a fixed string.
func armRoots(t *testing.T) (arm0, arm1 string) {
	t.Helper()
	for _, k := range []string{"CHE-1", "CHE-2", "CHE-3", "CHE-4", "CHE-5", "CHE-6", "CHE-7", "CHE-8"} {
		if TrialArm(k) == 0 && arm0 == "" {
			arm0 = k
		}
		if TrialArm(k) == 1 && arm1 == "" {
			arm1 = k
		}
	}
	if arm0 == "" || arm1 == "" {
		t.Fatal("no key for one of the arms")
	}
	return
}

func TestBootPrefixForActiveOnlyInTreatmentArm(t *testing.T) {
	arm0, arm1 := armRoots(t)
	root := t.TempDir()
	on := Config{WorkspacesRoot: root, CostTrialLever: true}
	task := func(rootKey string) Task {
		return Task{AgentID: "id1", Agent: &AgentData{Name: "a"}, IssueIdentifier: "CHE-9", RootIssueIdentifier: rootKey}
	}
	stable := filepath.Join(root, ".stable-cwd", "id1")
	cases := []struct {
		name     string
		cfg      Config
		task     Task
		provider string
		bwrap    bool
		wantCwd  string
		wantOn   bool
	}{
		{"arm 1, lever on", on, task(arm1), "claude", true, stable, true},
		{"arm 0, lever on: control", on, task(arm0), "claude", true, "", false},
		{"arm 1, lever off: control", Config{WorkspacesRoot: root}, task(arm1), "claude", true, "", false},
		{"arm 1, other provider", on, task("" + arm1), "codex", true, "", false},
		{"arm 1, no bwrap: control", on, task(arm1), "claude", false, "", false},
		{"root unknown: control", on, task(""), "claude", true, "", false},
		{"no workspaces root: control", Config{CostTrialLever: true}, task(arm1), "claude", true, "", false},
	}
	for _, c := range cases {
		cwd, lever := c.cfg.bootPrefixFor(c.task, c.provider, c.bwrap)
		if cwd != c.wantCwd || lever != c.wantOn {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, cwd, lever, c.wantCwd, c.wantOn)
		}
	}
}

// A listed agent keeps its stable cwd and the arm-0 brief layout: the lever is
// about the brief, the existing list is about the cwd only.
func TestBootPrefixForKeepsListedAgentStableCwdInControl(t *testing.T) {
	arm0, _ := armRoots(t)
	root := t.TempDir()
	cfg := Config{WorkspacesRoot: root, CostTrialLever: true, StableCwdAgents: []string{"a"}}
	task := Task{AgentID: "id1", Agent: &AgentData{Name: "a"}, IssueIdentifier: "CHE-9", RootIssueIdentifier: arm0}
	cwd, lever := cfg.bootPrefixFor(task, "claude", true)
	if cwd != filepath.Join(root, ".stable-cwd", "id1") || lever {
		t.Errorf("got (%q, %v), want the listed cwd and no lever", cwd, lever)
	}
}

func armBriefCtx(issue string) execenv.TaskContextForEnv {
	return execenv.TaskContextForEnv{
		IssueID: issue, AgentID: "agent-1", AgentName: "a", AgentInstructions: "Do work.",
		RequestingUserName: "Ann", RequestingUserProfileDescription: "PM " + issue,
		ProjectID: "p-" + issue, ProjectTitle: "Title " + issue,
	}
}

// Arm 0 keeps the workdir CLAUDE.md and no brief file; arm 1 has the brief
// file, no CLAUDE.md, and the per-task part for the turn prompt.
func TestDeliverBriefLayoutByArm(t *testing.T) {
	ctx := armBriefCtx("issue-a")

	work0, root0 := t.TempDir(), t.TempDir()
	brief, path, perTask, err := deliverBrief("claude", ctx, work0, root0, false)
	if err != nil {
		t.Fatal(err)
	}
	if path != "" || perTask != "" || brief == "" {
		t.Errorf("arm 0: got path=%q perTask=%q", path, perTask)
	}
	if _, err := os.Stat(filepath.Join(work0, "CLAUDE.md")); err != nil {
		t.Errorf("arm 0 must write CLAUDE.md: %v", err)
	}

	work1, root1 := t.TempDir(), t.TempDir()
	_, path, perTask, err = deliverBrief("claude", ctx, work1, root1, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work1, "CLAUDE.md")); err == nil {
		t.Error("arm 1 must not write CLAUDE.md")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("arm 1 brief file: %v", err)
	}
	stable, wantPerTask := execenv.SplitRuntimeBrief("claude", ctx)
	if string(data) != stable || perTask != wantPerTask || perTask == "" {
		t.Errorf("arm 1 file/perTask do not match the split")
	}
	if strings.Contains(string(data), "Title issue-a") {
		t.Error("brief file holds per-task text")
	}
}

// A stale marker block from an earlier arm-0 run must not duplicate the brief.
func TestDeliverBriefArm1RemovesStaleClaudeMd(t *testing.T) {
	ctx := armBriefCtx("issue-a")
	work, root := t.TempDir(), t.TempDir()
	if _, err := execenv.InjectRuntimeConfig(work, "claude", ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := deliverBrief("claude", ctx, work, root, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, "CLAUDE.md")); err == nil {
		t.Error("stale CLAUDE.md block survived")
	}
}

func TestBuildPromptAppendsTaskBriefOnlyWhenGiven(t *testing.T) {
	task := Task{IssueID: "i1"}
	if got := BuildPrompt(task, "claude"); strings.Contains(got, "PER-TASK-BRIEF") {
		t.Error("no option, no brief")
	}
	got := BuildPrompt(task, "claude", WithTaskBrief("## Project Context\n\nPER-TASK-BRIEF\n"))
	if !strings.Contains(got, "PER-TASK-BRIEF") {
		t.Errorf("prompt lost the per-task brief:\n%s", got)
	}
}

func TestCostTrialLeverConfigDefaultOff(t *testing.T) {
	t.Setenv("MULTICA_COST_TRIAL_LEVER", "")
	if got := costTrialLeverFromEnv(); got {
		t.Error("default must be off")
	}
	t.Setenv("MULTICA_COST_TRIAL_LEVER", "1")
	if !costTrialLeverFromEnv() {
		t.Error("1 must turn it on")
	}
}
