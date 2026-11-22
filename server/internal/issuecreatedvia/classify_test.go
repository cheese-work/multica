package issuecreatedvia

import "testing"

func TestClassify(t *testing.T) {
	chat := &TaskFacts{HasChatSession: true}
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"1 quick_create origin", Input{OriginType: OriginQuickCreate, CreatorAgent: true, Task: chat}, Dialog},
		{"2 autopilot origin", Input{OriginType: OriginAutopilot, CreatorAgent: true}, Autopilot},
		{"2 task with autopilot run", Input{CreatorAgent: true, Task: &TaskFacts{Autopilot: true}}, Autopilot},
		{"3 chat task, no same-run parent", Input{CreatorAgent: true, Task: chat}, DM},
		{"4 chat task, parent from same run", Input{CreatorAgent: true, Task: chat, ParentMadeBySameTask: true}, Agent},
		{"5 member comment trigger", Input{CreatorAgent: true, Task: &TaskFacts{EvidenceKind: "comment", TriggerCommentByMember: true}}, Comment},
		{"5 agent comment trigger", Input{CreatorAgent: true, Task: &TaskFacts{EvidenceKind: "comment"}}, Agent},
		{"5b legacy NULL kind, member comment", Input{CreatorAgent: true, Task: &TaskFacts{EvidenceKindNull: true, TriggerCommentByMember: true}}, Comment},
		{"6 wakeup copies a member comment", Input{CreatorAgent: true, Task: &TaskFacts{EvidenceKind: "issue_wakeup", TriggerCommentByMember: true}}, Agent},
		{"6 assignment task", Input{CreatorAgent: true, Task: &TaskFacts{EvidenceKind: "issue_assignment"}}, Agent},
		{"6 legacy task, no chat, no comment", Input{CreatorAgent: true, Task: &TaskFacts{EvidenceKindNull: true}}, Agent},
		{"6 no task (lookup failed)", Input{CreatorAgent: true}, Agent},
		{"8 member via CLI", Input{Channel: "cli"}, CLI},
		{"9 member via web", Input{Channel: "web"}, Form},
		{"9 member, unknown channel", Input{}, Form},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.in); got != tc.want {
				t.Fatalf("Classify = %q, want %q", got, tc.want)
			}
		})
	}
}
