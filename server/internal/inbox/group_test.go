package inbox

import (
	"context"
	"os"
	"regexp"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-6398 group-mute tests, ported from main's group_test.go onto the v0.10
// registry (typeGroups/Muted) that replaced group.go.

// TestIsNotifMutedGroupSplit pins the contract the `mentions` group exists for:
// muting comment VOLUME must never silence someone asking for you BY NAME.
func TestIsNotifMutedGroupSplit(t *testing.T) {
	cases := []struct {
		name      string
		prefs     map[string]string
		notifType string
		want      bool
	}{
		{"muted comments still delivers mentions", map[string]string{"comments": "muted"}, "mentioned", false},
		{"muted comments drops new_comment", map[string]string{"comments": "muted"}, "new_comment", true},
		{"muted mentions drops mentioned", map[string]string{"mentions": "muted"}, "mentioned", true},
		{"muted mentions still delivers new_comment", map[string]string{"mentions": "muted"}, "new_comment", false},
		{"both muted drops both", map[string]string{"comments": "muted", "mentions": "muted"}, "mentioned", true},
		{"empty prefs delivers mentions", map[string]string{}, "mentioned", false},
		{"explicit all delivers", map[string]string{"comments": "all"}, "new_comment", false},
		{"assignments group intact", map[string]string{"assignments": "muted"}, "issue_assigned", true},
		{"status group intact", map[string]string{"status_changes": "muted"}, "status_changed", true},
		{"muting comments leaves assignments alone", map[string]string{"comments": "muted"}, "issue_assigned", false},
		// CHE-6398 mappings.
		{"autopilot mutes issue_subscribed", map[string]string{"autopilot": "muted"}, "issue_subscribed", true},
		{"autopilot mutes autopilot_paused", map[string]string{"autopilot": "muted"}, "autopilot_paused", true},
		{"autopilot mutes autopilot_quota_exceeded", map[string]string{"autopilot": "muted"}, "autopilot_quota_exceeded", true},
		{"status_changes mutes children_done", map[string]string{"status_changes": "muted"}, "children_done", true},
		{"assignments mutes review_requested", map[string]string{"assignments": "muted"}, "review_requested", true},
		{"agent_activity mutes agent_blocked", map[string]string{"agent_activity": "muted"}, "agent_blocked", true},
		{"agent_activity mutes task_completed", map[string]string{"agent_activity": "muted"}, "task_completed", true},
		{"muting comments leaves children_done alone", map[string]string{"comments": "muted"}, "children_done", false},
		// Always-on types have no group.
		{"quick_create_done always delivered", map[string]string{"comments": "muted"}, "quick_create_done", false},
		{"machine_drained always delivered", map[string]string{"agent_activity": "muted", "autopilot": "muted"}, "machine_drained", false},
		{"dm_reply always delivered", map[string]string{"comments": "muted"}, "dm_reply", false},
		{"ci_watch always delivered", map[string]string{"comments": "muted"}, "ci_watch", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Muted(tc.prefs, tc.notifType); got != tc.want {
				t.Fatalf("Muted(%v, %q) = %v, want %v", tc.prefs, tc.notifType, got, tc.want)
			}
		})
	}
}

// TestEveryInboxTypeIsClassified fails when an item type has no group and is
// not a known always-on or decision type: such a type would be delivered
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
	// Always-on: no group switch by decision. dm_reply is owned by the DM
	// channel switches (CHE-1848); quick_create_* and machine_drained answer an
	// action the recipient started. Decision types bypass through isDecision.
	alwaysOn := map[string]bool{
		"dm_reply": true, "ci_watch": true, "quick_create_done": true, "quick_create_failed": true,
		"quick_create_unconfirmed": true, "machine_drained": true,
		// Written only by the server, always shown to the person who must act.
		"project_owner_joined": true, "app_action_requested": true, "app_action_reminder": true, "app_connect_requested": true,
	}
	decisionTypes := map[string]bool{"agent_decision_notice": true, "run_budget": true}
	// Go-only types: written by the server, absent from the TS union.
	all := []string{"agent_decision_notice", "dm_reply", "run_budget", "ci_watch"}
	for _, m := range types {
		all = append(all, string(m[1]))
	}
	var missing []string
	for _, ty := range all {
		if group, _ := groupOf(ty); group == "" && !alwaysOn[ty] && !decisionTypes[ty] {
			missing = append(missing, ty)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d inbox types have no group, always-on entry or bypass: %v", len(missing), missing)
	}
}

// Every fork producer reaches AllowsDelivery through CreateIfEligible, so one group
// mute must hold for types that no listener ever pre-checked.
func TestCreateIfEligibleGroupMute(test *testing.T) {
	recipientID := dbid.NewV7()
	for _, scenario := range []struct {
		name        string
		itemType    string
		preferences string
		details     string
		allowed     bool
	}{
		{name: "reaction with reactions muted", itemType: "reaction_added", preferences: `{"reactions":"muted"}`},
		{name: "reaction with comments muted", itemType: "reaction_added", preferences: `{"comments":"muted"}`, allowed: true},
		{name: "issue_subscribed with autopilot muted", itemType: "issue_subscribed", preferences: `{"autopilot":"muted"}`},
		{name: "autopilot_paused with autopilot muted", itemType: "autopilot_paused", preferences: `{"autopilot":"muted"}`},
		{name: "autopilot_quota_exceeded with autopilot muted", itemType: "autopilot_quota_exceeded", preferences: `{"autopilot":"muted"}`},
		{name: "children_done with status_changes muted", itemType: "children_done", preferences: `{"status_changes":"muted"}`},
		{name: "review_requested with assignments muted", itemType: "review_requested", preferences: `{"assignments":"muted"}`},
		{name: "agent_blocked with agent_activity muted", itemType: "agent_blocked", preferences: `{"agent_activity":"muted"}`},
		{name: "task_failed with agent_activity muted", itemType: "task_failed", preferences: `{"agent_activity":"muted"}`},
		{name: "delegated exhaustion bypasses agent_activity mute", itemType: "task_failed", preferences: `{"agent_activity":"muted"}`, details: `{"kind":"delegated_failure_exhausted"}`, allowed: true},
		{name: "run_budget bypasses agent_activity mute", itemType: "task_failed", preferences: `{"agent_activity":"muted"}`, details: `{"kind":"run_budget"}`, allowed: true},
		{name: "quick_create_done is always on", itemType: "quick_create_done", preferences: `{"comments":"muted","assignments":"muted","agent_activity":"muted"}`, allowed: true},
		{name: "machine_drained is always on", itemType: "machine_drained", preferences: `{"agent_activity":"muted","autopilot":"muted"}`, allowed: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			database := &deliveryDB{preferences: []byte(scenario.preferences)}
			params := db.CreateInboxItemParams{
				WorkspaceID: dbid.NewV7(), RecipientType: "member", RecipientID: recipientID,
				Type: scenario.itemType, Details: []byte(scenario.details),
			}
			_, inserted, err := CreateIfEligible(context.Background(), db.New(database), params)
			if err != nil {
				test.Fatal(err)
			}
			if inserted != scenario.allowed {
				test.Fatalf("inserted=%v, want %v", inserted, scenario.allowed)
			}
		})
	}
}

// The scope switch runs before the group check: an assigned issue still obeys
// its group mute.
func TestCreateIfEligibleScopeThenGroup(test *testing.T) {
	recipientID := dbid.NewV7()
	database := &deliveryDB{preferences: []byte(`{"unassigned_activity":"muted","status_changes":"muted"}`), assigneeType: "member", assigneeID: recipientID}
	_, inserted, err := CreateIfEligible(context.Background(), db.New(database), db.CreateInboxItemParams{
		WorkspaceID: dbid.NewV7(), RecipientType: "member", RecipientID: recipientID, IssueID: dbid.NewV7(), Type: "status_changed",
	})
	if err != nil || inserted {
		test.Fatalf("inserted=%v err=%v; want muted status change on my own issue dropped", inserted, err)
	}
}
