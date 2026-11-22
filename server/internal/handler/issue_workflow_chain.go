package handler

import (
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// chainsSameOwner reports whether a status write is an agent's run carrying
// on into the next step instead of handing the issue over. The entered step
// must set chain_same_owner, the caller's live run must belong to the agent
// the write leaves as assignee, and the entered step's handler type must equal
// that of the step the run is in (a role change still starts a run). Anything
// else, including a missing step on the run, a member caller or another
// assignee, keeps the handoff's run.
func chainsSameOwner(actorType string, run *db.AgentTaskQueue, handoff *workflowHandoff, params db.UpdateIssueParams) bool {
	if actorType != "agent" || run == nil || handoff == nil || !handoff.step.ChainSameOwner {
		return false
	}
	if run.Status != "running" && run.Status != "dispatched" {
		return false
	}
	if params.AssigneeType.String != "agent" || params.AssigneeID != run.AgentID {
		return false
	}
	from, ok := handoff.def.Step(run.WorkflowStep.String)
	return ok && from.Handler.Type == handoff.step.Handler.Type
}
