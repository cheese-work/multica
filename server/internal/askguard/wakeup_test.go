package askguard

import "testing"

func TestWakeupRuleMemberEligible(t *testing.T) {
	comment := []string{"comment.created"}
	cases := []struct {
		name string
		rule WakeupRule
		want bool
	}{
		{"unfiltered comment", WakeupRule{Kind: "event", EventTypes: comment}, true},
		{"member filter", WakeupRule{Kind: "event", EventTypes: comment, FilterActorType: "member", FilterActorID: "m"}, true},
		{"issue change", WakeupRule{Kind: "event", EventTypes: []string{"issue.status_changed"}}, true},
		{"reaction", WakeupRule{Kind: "event", EventTypes: []string{"reaction.added"}}, true},
		{"attachment", WakeupRule{Kind: "event", EventTypes: []string{"attachment.attached"}}, true},
		{"mixed task and comment", WakeupRule{Kind: "event", EventTypes: []string{"task.completed", "comment.created"}}, true},
		{"any agent", WakeupRule{Kind: "event", EventTypes: comment, FilterActorType: "agent"}, false},
		{"one agent", WakeupRule{Kind: "event", EventTypes: comment, FilterActorType: "agent", FilterActorID: "a"}, false},
		{"task event", WakeupRule{Kind: "event", EventTypes: []string{"task.completed"}}, false},
		{"condition", WakeupRule{Kind: "event", Condition: true}, false},
		{"timer", WakeupRule{Kind: "at"}, false},
		{"recurring", WakeupRule{Kind: "every"}, false},
	}
	for _, tc := range cases {
		if got := tc.rule.MemberEligible(); got != tc.want {
			t.Errorf("%s: MemberEligible = %v, want %v", tc.name, got, tc.want)
		}
	}
}
