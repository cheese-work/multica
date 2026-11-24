package inbox

import (
	"os"
	"regexp"
	"testing"
)

// TestIsNotifMutedGroupSplit pins the contract the `mentions` group exists for:
// muting comment VOLUME must never silence someone asking for you BY NAME.
// `new_comment` and `mentioned` shared the `comments` group until #6468, so a
// user who muted comment churn also lost every mention — and a muted item is
// never created, so it was unrecoverable rather than merely delayed.
func TestIsNotifMutedGroupSplit(t *testing.T) {
	cases := []struct {
		name      string
		prefs     map[string]string
		notifType string
		want      bool
	}{
		// The regression this split fixes.
		{"muted comments still delivers mentions", map[string]string{"comments": "muted"}, "mentioned", false},
		{"muted comments drops new_comment", map[string]string{"comments": "muted"}, "new_comment", true},
		{"muted mentions drops mentioned", map[string]string{"mentions": "muted"}, "mentioned", true},
		{"muted mentions still delivers new_comment", map[string]string{"mentions": "muted"}, "new_comment", false},
		{"both muted drops both", map[string]string{"comments": "muted", "mentions": "muted"}, "mentioned", true},

		// Absent key means the default "all" — preferences are stored sparse,
		// which is why existing `{"comments":"muted"}` rows need no migration.
		{"empty prefs delivers mentions", map[string]string{}, "mentioned", false},
		{"empty prefs delivers comments", map[string]string{}, "new_comment", false},
		{"explicit all delivers", map[string]string{"comments": "all"}, "new_comment", false},

		// Other groups are unaffected by the split.
		{"assignments group intact", map[string]string{"assignments": "muted"}, "issue_assigned", true},
		{"status group intact", map[string]string{"status_changes": "muted"}, "status_changed", true},
		{"muting comments leaves assignments alone", map[string]string{"comments": "muted"}, "issue_assigned", false},

		// CHE-6400: assignee churn has its own switch; "Assigned to me" keeps
		// issue_assigned.
		{"muted assignee_changes drops assignee_changed", map[string]string{"assignee_changes": "muted"}, "assignee_changed", true},
		{"muted assignee_changes drops unassigned", map[string]string{"assignee_changes": "muted"}, "unassigned", true},
		{"muted assignee_changes still delivers issue_assigned", map[string]string{"assignee_changes": "muted"}, "issue_assigned", false},
		{"muted assignments still delivers assignee_changed", map[string]string{"assignments": "muted"}, "assignee_changed", false},
		{"muted assignments still delivers unassigned", map[string]string{"assignments": "muted"}, "unassigned", false},

		// Types outside the map are always delivered.
		{"unconfigurable type always delivered", map[string]string{"comments": "muted"}, "quick_create_done", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNotifMuted(tc.prefs, tc.notifType); got != tc.want {
				t.Fatalf("isNotifMuted(%v, %q) = %v, want %v",
					tc.prefs, tc.notifType, got, tc.want)
			}
		})
	}
}

// TestEveryInboxTypeIsClassified fails when an item type has no group, no
// always-on entry and no decision bypass: such a type would be delivered
// regardless of the user's settings by accident rather than by decision.
func TestEveryInboxTypeIsClassified(t *testing.T) {
	src, err := os.ReadFile("../../../packages/core/types/inbox.ts")
	if err != nil {
		t.Fatalf("read InboxItemType union (moved?): %v", err)
	}
	src = regexp.MustCompile(`(?m)//.*$`).ReplaceAll(src, nil) // comments may hold ";"
	union := regexp.MustCompile(`(?s)export type InboxItemType =(.*?);`).FindSubmatch(src)
	if union == nil {
		t.Fatal("InboxItemType union not found in packages/core/types/inbox.ts")
	}
	types := regexp.MustCompile(`\|\s*"([a-z_]+)"`).FindAllSubmatch(union[1], -1)
	if len(types) == 0 {
		t.Fatal("no types parsed from InboxItemType")
	}
	// Go-only types: written by the server, absent from the TS union.
	all := []string{"agent_decision_notice", "dm_reply", "run_budget", "ci_watch"}
	for _, m := range types {
		all = append(all, string(m[1]))
	}
	var missing []string
	for _, ty := range all {
		_, grouped := typeGroup[ty]
		if !grouped && !alwaysOn[ty] && !decisionTypes[ty] {
			missing = append(missing, ty)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d inbox types have no group, always-on entry or bypass: %v", len(missing), missing)
	}
}
