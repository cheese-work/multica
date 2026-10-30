package agentpreset

import (
	"reflect"
	"testing"
)

func sp(s string) *string { return &s }

var claudeAgent = Agent{Provider: "claude", Model: "claude-opus-5-5", ThinkingLevel: "high"}

func preset(name string) Preset { return Preset{Name: name, ThinkingLevel: sp("low")} }

func TestResolve(t *testing.T) {
	const agentID, squadID = "agent-1", "squad-1"
	fast, slow := preset("fast"), preset("slow")
	def := preset("def")
	def.IsDefault = true

	tests := []struct {
		name       string
		in         Input
		wantName   string
		wantSource Source
		wantSkips  []Skip
	}{
		{
			name:       "P1 nothing set resolves to base",
			in:         Input{Agent: claudeAgent, Presets: []Preset{fast}},
			wantName:   BaseName,
			wantSource: SourceBase,
		},
		{
			name:       "P2 default preset wins over base",
			in:         Input{Agent: claudeAgent, Presets: []Preset{fast, def}},
			wantName:   "def",
			wantSource: SourceAgentDefault,
		},
		{
			name: "P3 workflow step preset beats agent default",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{fast, def},
				Step: &StepHandler{Type: "agent", ID: agentID, Preset: "fast"},
				Task: Task{AgentID: agentID},
			},
			wantName:   "fast",
			wantSource: SourceWorkflowStep,
		},
		{
			name: "P3b step handler is another agent: ignored",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{fast, def},
				Step: &StepHandler{Type: "agent", ID: "other", Preset: "fast"},
				Task: Task{AgentID: agentID},
			},
			wantName:   "def",
			wantSource: SourceAgentDefault,
		},
		{
			name: "P3c squad handler applies to that squad's leader task",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{fast, def},
				Step: &StepHandler{Type: "squad", ID: squadID, Preset: "fast"},
				Task: Task{AgentID: agentID, SquadID: squadID, IsLeaderTask: true},
			},
			wantName:   "fast",
			wantSource: SourceWorkflowStep,
		},
		{
			name: "P3c squad handler ignores a member task of that squad",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{fast, def},
				Step: &StepHandler{Type: "squad", ID: squadID, Preset: "fast"},
				Task: Task{AgentID: agentID, SquadID: squadID},
			},
			wantName:   "def",
			wantSource: SourceAgentDefault,
		},
		{
			name: "P3c squad handler ignores another squad's leader task",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{fast, def},
				Step: &StepHandler{Type: "squad", ID: squadID, Preset: "fast"},
				Task: Task{AgentID: agentID, SquadID: "other", IsLeaderTask: true},
			},
			wantName:   "def",
			wantSource: SourceAgentDefault,
		},
		{
			name: "P4 issue label beats workflow step",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{fast, slow},
				IssueLabels: []string{"slow"},
				Step:        &StepHandler{Type: "agent", ID: agentID, Preset: "fast"},
				Task:        Task{AgentID: agentID},
			},
			wantName:   "slow",
			wantSource: SourceIssueLabel,
		},
		{
			name: "P5 DM beats issue label",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{fast, slow},
				DM: sp("fast"), IssueLabels: []string{"slow"},
			},
			wantName:   "fast",
			wantSource: SourceDM,
		},
		{
			name: "P5 explicit base DM beats a non-base agent default",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{def},
				DM: sp(BaseName),
			},
			wantName:   BaseName,
			wantSource: SourceDM,
		},
		{
			name: "P5 explicit base issue label beats the agent default",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{def},
				IssueLabels: []string{BaseName},
			},
			wantName:   BaseName,
			wantSource: SourceIssueLabel,
		},
		{
			name: "P6 unknown DM name: next level wins and the skip is recorded",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{fast},
				DM: sp("ghost"), IssueLabels: []string{"fast"},
			},
			wantName:   "fast",
			wantSource: SourceIssueLabel,
			wantSkips:  []Skip{{Level: SourceDM, Name: "ghost", Reason: ReasonUnknownPreset}},
		},
		{
			name: "P7 two preset labels are skipped as ambiguous",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{fast, slow, def},
				IssueLabels: []string{"fast", "slow"},
			},
			wantName:   "def",
			wantSource: SourceAgentDefault,
			wantSkips:  []Skip{{Level: SourceIssueLabel, Reason: ReasonAmbiguous}},
		},
		{
			name: "P9 preset deleted after enqueue: next level wins",
			in: Input{
				Agent: claudeAgent, Presets: []Preset{def},
				DM: sp("fast"),
			},
			wantName:   "def",
			wantSource: SourceAgentDefault,
			wantSkips:  []Skip{{Level: SourceDM, Name: "fast", Reason: ReasonUnknownPreset}},
		},
		{
			name: "stale default falls back to base and records the skip",
			in: Input{
				Agent:   claudeAgent,
				Presets: []Preset{{Name: "bad", IsDefault: true, ContextArgs: []string{"-c", "model_context_window=1"}}},
			},
			wantName:   BaseName,
			wantSource: SourceBase,
			wantSkips:  []Skip{{Level: SourceAgentDefault, Name: "bad", Reason: ReasonContextArgs}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.in)
			if got.Name != tc.wantName || got.Source != tc.wantSource {
				t.Fatalf("got %s/%s, want %s/%s", got.Name, got.Source, tc.wantName, tc.wantSource)
			}
			if !reflect.DeepEqual(got.Skips, tc.wantSkips) {
				t.Fatalf("skips = %+v, want %+v", got.Skips, tc.wantSkips)
			}
			if (got.Name == BaseName) != (got.Preset == nil) {
				t.Fatalf("Preset nil mismatch for %s", got.Name)
			}
		})
	}
}

func TestResolveP8NullFieldsInheritAgentRow(t *testing.T) {
	agent := Agent{Provider: "codex", Model: "gpt-6.1-sol", ThinkingLevel: "high", ServiceTier: "fast"}
	got := Resolve(Input{
		Agent:   agent,
		Presets: []Preset{{Name: "m", Model: sp("gpt-6-luna")}},
		DM:      sp("m"),
	})
	want := Settings{Model: "gpt-6-luna", ThinkingLevel: "high", ServiceTier: "fast"}
	if !reflect.DeepEqual(got.Settings, want) {
		t.Fatalf("settings = %+v, want %+v", got.Settings, want)
	}
}

func TestResolveKeepsExplicitEmptyContextArgs(t *testing.T) {
	got := Resolve(Input{
		Agent:   claudeAgent,
		Presets: []Preset{{Name: "clear", ContextArgs: []string{}}},
		DM:      sp("clear"),
	})
	if got.Name != "clear" || got.Settings.ContextArgs == nil || len(got.Settings.ContextArgs) != 0 {
		t.Fatalf("got %s with context args %#v, want clear with non-nil empty", got.Name, got.Settings.ContextArgs)
	}
}
