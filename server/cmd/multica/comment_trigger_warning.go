package main

import (
	"fmt"
	"io"
)

// warnBlockedTriggerOutcomes prints one warning per @agent / @squad mention the
// server refused to run (CHE-1418), so a suppressed trigger is never silent.
func warnBlockedTriggerOutcomes(w io.Writer, result map[string]any) {
	outcomes, _ := result["trigger_outcomes"].([]any)
	for _, raw := range outcomes {
		outcome, _ := raw.(map[string]any)
		if outcome["status"] != "blocked" {
			continue
		}
		fmt.Fprintf(w, "Warning: mention of %v %v started no run (%v).\n",
			outcome["target_type"], outcome["target_id"], outcome["reason_code"])
	}
}
