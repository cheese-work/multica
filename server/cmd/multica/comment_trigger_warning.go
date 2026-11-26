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
			fmt.Fprintf(w, "Note: review of %v %v is queued behind a full stage; it starts when a slot frees. "+
				"If no run exists after about 30 minutes, start it with: multica issue wakeup create --until-status <status> --agent-id %v. Do not re-mention or re-assign.\n",
				outcome["target_type"], outcome["target_id"], outcome["target_id"])
			continue
		}
		if outcome["status"] != "blocked" {
			continue
		}
		fmt.Fprintf(w, "Warning: trigger for %v %v started no run (%v).\n",
			outcome["target_type"], outcome["target_id"], outcome["reason_code"])
	}
}
