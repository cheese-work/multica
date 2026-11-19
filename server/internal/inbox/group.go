package inbox

// typeGroup maps each InboxItemType to a user-configurable preference group.
// A type with no entry here must be in alwaysOn or decisionTypes
// (TestEveryInboxTypeIsClassified).
var typeGroup = map[string]string{
	"issue_assigned":     "assignments",
	"unassigned":         "assignments",
	"assignee_changed":   "assignments",
	"status_changed":     "status_changes",
	"new_comment":        "comments",
	"mentioned":          "mentions",
	"priority_changed":   "updates",
	"start_date_changed": "updates",
	"due_date_changed":   "updates",
	"task_completed":     "agent_activity",
	"task_failed":        "agent_activity",
	"agent_blocked":      "agent_activity",
	"agent_completed":    "agent_activity",
	"reaction_added":     "reactions",
	// issue_subscribed has no switch of its own: it is the autopilot-created
	// issue notice, so it follows the autopilot group.
	"issue_subscribed":         "autopilot",
	"autopilot_paused":         "autopilot",
	"autopilot_quota_exceeded": "autopilot",
	"children_done":            "status_changes",
	"review_requested":         "assignments",
}

// alwaysOn types have no group switch. dm_reply is owned by the DM channel
// switches (CHE-1848), not by an inbox group.
// quick_create_* and machine_drained answer an action the recipient started
// (quick create, pause machine); no other surface reports the result.
var alwaysOn = map[string]bool{
	"dm_reply":                 true,
	"ci_watch":                 true, // Go-only; no group today, kept as is (not in plan r2)
	"quick_create_done":        true,
	"quick_create_failed":      true,
	"quick_create_unconfirmed": true,
	"machine_drained":          true,
}

// decisionTypes bypass every preference through isDecision.
var decisionTypes = map[string]bool{"agent_decision_notice": true, "run_budget": true}

// isNotifMuted reports whether the user's preferences mute the type's group.
func isNotifMuted(prefs map[string]string, notifType string) bool {
	group, ok := typeGroup[notifType]
	return ok && prefs[group] == "muted"
}
