package service

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// WorkflowSplit is the CHE-5238 A/B setting stored on a project: between
// StartsAt and EndsAt a new issue may follow arm B's workflow instead of the
// project's own (arm A).
type WorkflowSplit struct {
	ArmBWorkflowID      string    `json:"arm_b_workflow_id"`
	StartsAt            time.Time `json:"starts_at"`
	EndsAt              time.Time `json:"ends_at"`
	ExcludedAssigneeIDs []string  `json:"excluded_assignee_ids"`
	ExcludedSquadIDs    []string  `json:"excluded_squad_ids"`
}

func DecodeWorkflowSplit(raw []byte) *WorkflowSplit {
	if len(raw) == 0 {
		return nil
	}
	var s WorkflowSplit
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

// Activity actions and rules written when a split decides an issue.
const (
	SplitActivityArmB     = "workflow_split"
	SplitActivityExcluded = "workflow_split_excluded"
	splitRuleParity       = "parity"
	splitRuleParent       = "parent"
	splitRuleAndroid      = "android"
)

// splitChoice is what the split decided for a new issue: the override to
// store (invalid = none) and the activity to log ("" = nothing to log).
type splitChoice struct {
	WorkflowID pgtype.UUID
	Action     string
	Rule       string
}

// chooseSplit applies the rules in order: an inactive split or an explicit
// workflow leaves the issue alone; an excluded assignee gets none; a child takes
// its parent's arm B or none and never draws parity; a root draws by number
// (even = arm B). The assignee is the one the caller named, not a step handler.
func chooseSplit(split *WorkflowSplit, now time.Time, explicit bool, assigneeType pgtype.Text, assigneeID pgtype.UUID, hasParent bool, parentWorkflow pgtype.UUID, number int32) splitChoice {
	if split == nil || explicit || now.Before(split.StartsAt) || !now.Before(split.EndsAt) {
		return splitChoice{}
	}
	armB, err := util.ParseUUID(split.ArmBWorkflowID)
	if err != nil {
		return splitChoice{}
	}
	if assigneeID.Valid {
		id := util.UUIDToString(assigneeID)
		if (assigneeType.String == "agent" && slices.Contains(split.ExcludedAssigneeIDs, id)) ||
			(assigneeType.String == "squad" && slices.Contains(split.ExcludedSquadIDs, id)) {
			return splitChoice{Action: SplitActivityExcluded, Rule: splitRuleAndroid}
		}
	}
	if hasParent {
		if parentWorkflow == armB {
			return splitChoice{WorkflowID: armB, Action: SplitActivityArmB, Rule: splitRuleParent}
		}
		return splitChoice{}
	}
	if number%2 == 0 {
		return splitChoice{WorkflowID: armB, Action: SplitActivityArmB, Rule: splitRuleParity}
	}
	return splitChoice{}
}
