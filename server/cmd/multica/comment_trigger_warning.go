package main

import (
	"fmt"
	"io"
)

// warnBlockedTriggerOutcomes prints one warning per @agent / @squad mention the
// server refused to run (CHE-1418), so a suppressed trigger is never silent.
// A mention deferred behind a full stage is already queued (CHE-7011): it gets a note, not a warning.
func warnBlockedTriggerOutcomes(w io.Writer, result map[string]any) {
	outcomes, _ := result["trigger_outcomes"].([]any)
	for _, raw := range outcomes {
		outcome, _ := raw.(map[string]any)
		if outcome["status"] == "deferred" && outcome["reason_code"] == "stage_queue_full" {
			fmt.Fprintf(w, "Note: review of %v %v is queued behind a full stage; it starts when a slot frees (checked every 30 s). "+
				"To re-check now, name the reviewer again: multica issue assign %v --to-id %v. "+
				"That goes through the same stage limit and joins the queued entry, so it starts no second run. "+
				"Do not use a status wakeup: the issue is already in that status.\n",
				outcome["target_type"], outcome["target_id"], retriggerIssueRef(result), outcome["target_id"])
			continue
		}
		if outcome["status"] != "blocked" {
			continue
		}
		fmt.Fprintf(w, "Warning: trigger for %v %v started no run (%v).\n",
			outcome["target_type"], outcome["target_id"], outcome["reason_code"])
	}
}

// retriggerIssueRef names the issue in the re-assign command: a comment
// response carries issue_id, an issue response its identifier.
func retriggerIssueRef(result map[string]any) string {
	if id := strVal(result, "issue_id"); id != "" {
		return id
	}
	return issueDisplayKey(result)
}
