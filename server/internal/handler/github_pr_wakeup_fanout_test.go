package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestMergedPRWakeupFailureDoesNotStopFanoutOrPublish(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := newPRMergeWebhookFixture(t, 527120)
	childAgent := createHandlerTestAgent(t, "PR merge fanout child", nil)
	parentAgent := createHandlerTestAgent(t, "PR merge fanout parent", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", childAgent)
	setIssueAssigneeDirect(t, fx.parent.ID, "agent", parentAgent)
	issueIDs := []pgtype.UUID{parseUUID(fx.child.ID), parseUUID(fx.parent.ID)}
	handler := *testHandler
	handler.Bus = events.New()
	published := make(chan events.Event, 1)
	handler.Bus.Subscribe(protocol.EventPullRequestUpdated, func(event events.Event) {
		published <- event
	})
	failure := errors.New("injected PR wakeup dispatch failure")
	wakeup := service.IssueWakeupService{Tasks: handler.TaskService}
	err := dispatchMergedPRWakeups(issueIDs, func(issueID pgtype.UUID) error {
		if issueID == issueIDs[0] {
			return failure
		}
		return wakeup.TriggerPullRequestWakeup(context.Background(), issueID, service.PullRequestWakeupInput{
			Rule: service.SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 527120,
			URL: "https://github.com/acme/widget/pull/527120", MergeCommit: "merge-fanout-test",
		})
	}, func() {
		handler.publish(protocol.EventPullRequestUpdated, testWorkspaceID, "system", "", map[string]any{"linked_issue_ids": []string{fx.child.ID, fx.parent.ID}})
	})
	if !errors.Is(err, failure) {
		t.Fatalf("fanout error = %v, want injected dispatch failure", err)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule='pr_merged' AND r.event_type='pr.merged'`, fx.parent.ID); got != 1 {
		t.Fatalf("later linked issue merge receipts = %d, want 1", got)
	}
	select {
	case event := <-published:
		if event.Type != protocol.EventPullRequestUpdated || event.WorkspaceID != testWorkspaceID {
			t.Fatalf("published event = %+v, want workspace pull-request update", event)
		}
	default:
		t.Fatal("committed PR update was not published after a dispatch error")
	}
}
