package handler

import (
	"context"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// captureLeaderFallbackTriggers writes the shadow ledger row (CHE-1918) for
// every trigger that resolved to a squad leader, before steering, folding or
// enqueueing touches the queue. It returns the captured events keyed by
// executing agent id. Capture only: the triggers are not read-modified here,
// and a failed write leaves them and the comment as they were.
func (h *Handler) captureLeaderFallbackTriggers(ctx context.Context, issue db.Issue, comment db.Comment, authorType string, triggers []commentAgentTrigger) map[string]service.RunGateEvent {
	var gated map[string]service.RunGateEvent
	headSHA := ""
	headLoaded := false
	for _, trigger := range triggers {
		if trigger.Squad == nil {
			continue
		}
		if !headLoaded {
			headSHA, headLoaded = h.TaskService.ResolveIssueReviewSHA(ctx, issue.ID), true
		}
		agentID := uuidToString(trigger.Agent.ID)
		ev := service.RunGateEvent{
			Issue: issue, Source: service.RunGateSourceComment, EventKey: uuidToString(comment.ID), CandidateKey: agentID,
			Origin: string(trigger.Source), EventTime: comment.CreatedAt.Time, PRHeadSHA: headSHA,
			AgentID: trigger.Agent.ID, SquadID: trigger.Squad.ID, AuthorType: authorType,
			NonLeaderResp: trigger.NonLeaderAgentReply, CommentID: comment.ID,
		}
		service.CaptureRunGateEvent(ctx, h.Queries, ev)
		if gated == nil {
			gated = make(map[string]service.RunGateEvent)
		}
		gated[agentID] = ev
	}
	return gated
}

// recordLeaderFallbackAdmissions copies onto each captured event the
// disposition the enqueue path itself returned. A trigger with no result (the
// composer opted it out) keeps an empty disposition: it is never inferred.
func (h *Handler) recordLeaderFallbackAdmissions(ctx context.Context, gated map[string]service.RunGateEvent, enqueued map[string]commentEnqueueResult) {
	for agentID, ev := range gated {
		if result, ok := enqueued[agentID]; ok {
			service.RecordRunGateAdmission(ctx, h.Queries, ev, string(result.status), string(result.reason))
		}
	}
}
