package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestAskedDecisionRecordFromAnotherDM(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	sessionID := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "Another DM"})
	messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "Use tomorrow's window.", "sent_via": "interactive_jwt"})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, fixture.chatTaskID)
	input := fixture.recordInput()
	input.MessageIDs = []pgtype.UUID{parseUUID(messageID)}
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	if err != nil {
		test.Fatalf("the named human's answer from another DM was rejected: %v", err)
	}
	if result.Request.Status != "answered" || result.Answer.ContentSnapshot != "Use tomorrow's window." || result.Answer.SourceChatSessionID != parseUUID(sessionID) {
		test.Fatalf("external answer lost its source: %+v", result.Answer)
	}
}

func TestAskedDecisionRecordFromIssueComment(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	sourceID := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "member", "author_id": fixture.humanID, "type": "comment", "content": "  Choose tomorrow.\nDo not deploy now.  "})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, fixture.chatTaskID)
	request := withIssueParam(fixture.recordRequest(), fixture.issueID)
	request.Method = http.MethodPost
	request.Body = ioBody(`{"decision_record":true,"parent_id":"` + fixture.parentID + `","source_comment_id":"` + sourceID + `"}`)
	testutil.Call(test, testHandler.CreateComment, request).Want(http.StatusCreated)
}

func TestAskedDecisionRecordFromOtherAgentsDM(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	agentID := createHandlerTestAgent(test, "Another DM agent", []byte("[]"))
	sessionID := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": fixture.humanID, "title": "Other agent DM"})
	messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "Proceed tomorrow.", "sent_via": "interactive_jwt"})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, fixture.chatTaskID)
	input := fixture.recordInput()
	input.MessageIDs = []pgtype.UUID{parseUUID(messageID)}
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	if err != nil || result.Answer.SourceChatSessionID != parseUUID(sessionID) {
		test.Fatalf("another agent's DM lost authenticated human provenance: %+v %v", result, err)
	}
}

func TestAskedDecisionRecordCommentFromIssueTaskKeepsSnapshot(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	otherIssue := dbfx.Issue(test, "Human replied on another issue")
	sourceID := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": otherIssue, "author_type": "member", "author_id": fixture.humanID, "content": "  Hold.\nDo not deploy.  "})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, fixture.taskID)
	input := decisionRecordInput{ParentID: parseUUID(fixture.parentID), SourceCommentID: parseUUID(sourceID)}
	result, err := testHandler.createAskedDecisionRecord(fixture.request(), fixture.issue, input)
	if err != nil {
		test.Fatal(err)
	}
	if result.Answer.SourceCommentID != parseUUID(sourceID) || result.Answer.SourceChatSessionID.Valid || len(result.Answer.ChatMessageIds) != 0 || result.Answer.ContentSnapshot != "  Hold.\nDo not deploy.  " || result.Answer.WakeAt.Valid {
		test.Fatalf("wrong comment provenance: %+v", result.Answer)
	}
	dbfx.Exec(test, `UPDATE comment SET content = 'Edited later.' WHERE id = $1`, sourceID)
	replay, err := testHandler.createAskedDecisionRecord(fixture.request(), fixture.issue, input)
	if err != nil || !replay.Replayed || replay.Answer.ID != result.Answer.ID || replay.Answer.ContentSnapshot != result.Answer.ContentSnapshot {
		test.Fatalf("retry rewrote the source snapshot: %+v %v", replay, err)
	}
	dbfx.Exec(test, `UPDATE comment SET deleted_at = clock_timestamp() WHERE id = $1`, sourceID)
	withdrawn, err := testHandler.CheckDecisionSource(context.Background(), fixture.issue.WorkspaceID, fixture.decision.ID)
	if err != nil || !withdrawn.Answer.DisclaimedAt.Valid || withdrawn.Request.Status != "open" {
		test.Fatalf("source deletion did not retract the pending answer: %+v %v", withdrawn, err)
	}
}

func TestAskedDecisionExternalSourceRefusals(test *testing.T) {
	for _, mutation := range []string{"wrong_human", "agent_author", "deleted", "foreign_workspace", "before_ask", "after_turn", "mixed_sources", "missing_parent"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			sourceID := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "member", "author_id": fixture.humanID, "content": "Yes."})
			dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, fixture.chatTaskID)
			input := decisionRecordInput{ParentID: parseUUID(fixture.parentID), SourceCommentID: parseUUID(sourceID)}
			switch mutation {
			case "wrong_human":
				dbfx.Exec(test, `UPDATE comment SET author_id = $1 WHERE id = $2`, testUserID, sourceID)
			case "agent_author":
				dbfx.Exec(test, `UPDATE comment SET author_type = 'agent', author_id = $1 WHERE id = $2`, fixture.agentID, sourceID)
			case "deleted":
				dbfx.Exec(test, `UPDATE comment SET deleted_at = clock_timestamp() WHERE id = $1`, sourceID)
			case "foreign_workspace":
				workspaceID := dbfx.Workspace(test, "External source workspace", "external-source-"+fixture.issueID)
				dbfx.Exec(test, `UPDATE comment SET workspace_id = $1 WHERE id = $2`, workspaceID, sourceID)
			case "before_ask":
				dbfx.Exec(test, `UPDATE comment SET created_at = 'epoch' WHERE id = $1`, sourceID)
			case "after_turn":
				dbfx.Exec(test, `UPDATE comment SET created_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, sourceID)
			case "mixed_sources":
				input.MessageIDs = fixture.recordInput().MessageIDs
			case "missing_parent":
				input.ParentID = pgtype.UUID{}
			}
			_, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status < 400 || refusal.status >= 500 {
				test.Fatalf("source boundary did not refuse %s: %v", mutation, err)
			}
			fixture.assertRecordCounts(test, 0)
		})
	}
}

func TestAskedDecisionExternalDMRejectsForeignWorkspace(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	workspaceID := dbfx.Workspace(test, "Foreign DM", "foreign-dm-"+fixture.issueID)
	sessionID := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": workspaceID, "agent_id": fixture.agentID, "creator_id": fixture.humanID, "title": "Foreign DM"})
	messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "Yes.", "sent_via": "interactive_jwt"})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, fixture.chatTaskID)
	input := fixture.recordInput()
	input.MessageIDs = []pgtype.UUID{parseUUID(messageID)}
	_, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || refusal.status != http.StatusForbidden {
		test.Fatalf("foreign-workspace source was accepted: %v", err)
	}
	stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.decision.ID})
	if err != nil || stored.Status != "open" {
		test.Fatalf("refusal changed the ask: %+v %v", stored, err)
	}
	fixture.assertRecordCounts(test, 0)
}
