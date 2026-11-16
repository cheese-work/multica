package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// addProjectSplitFlags adds the CHE-5238 A/B split flags to `project update`.
func addProjectSplitFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("split-arm-b", "", "Workflow ID of A/B arm B (needs --split-start and --split-end)")
	f.String("split-start", "", "Split start (RFC 3339)")
	f.String("split-end", "", "Split end (RFC 3339)")
	f.StringSlice("split-exclude-assignee", nil, "Assignee ID kept out of the split (repeatable)")
	f.StringSlice("split-exclude-squad", nil, "Squad ID kept out of the split (repeatable)")
	f.Bool("clear-split", false, "Remove the A/B split")
}

var projectSplitFlagNames = []string{"split-arm-b", "split-start", "split-end", "split-exclude-assignee", "split-exclude-squad"}

// projectSplitRequest reads the split flags. body is nil and clear false when
// none is set. The server validates the values.
func projectSplitRequest(cmd *cobra.Command) (body map[string]any, clear bool, err error) {
	f := cmd.Flags()
	anySet := false
	for _, name := range projectSplitFlagNames {
		anySet = anySet || f.Changed(name)
	}
	if f.Changed("clear-split") {
		if anySet {
			return nil, false, fmt.Errorf("--clear-split cannot be combined with other --split-* flags")
		}
		clear, _ = f.GetBool("clear-split")
		return nil, clear, nil
	}
	if !anySet {
		return nil, false, nil
	}
	armB, _ := f.GetString("split-arm-b")
	start, _ := f.GetString("split-start")
	end, _ := f.GetString("split-end")
	if armB == "" || start == "" || end == "" {
		return nil, false, fmt.Errorf("--split-arm-b, --split-start and --split-end go together")
	}
	assignees, _ := f.GetStringSlice("split-exclude-assignee")
	squads, _ := f.GetStringSlice("split-exclude-squad")
	return map[string]any{
		"arm_b_workflow_id":     armB,
		"starts_at":             start,
		"ends_at":               end,
		"excluded_assignee_ids": assignees,
		"excluded_squad_ids":    squads,
	}, false, nil
}
