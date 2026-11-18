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
			fmt.Fprintf(w, "Note: review of %v %v is queued behind a full stage and starts by itself; do not re-mention.\n",
				outcome["target_type"], outcome["target_id"])
			continue
		}
		if outcome["status"] != "blocked" {
			continue
		}
		fmt.Fprintf(w, "Warning: mention of %v %v started no run (%v).\n",
			outcome["target_type"], outcome["target_id"], outcome["reason_code"])
	}
}
