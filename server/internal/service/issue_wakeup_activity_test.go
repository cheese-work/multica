package service

import (
	"encoding/json"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A comment trigger's timeline entry carries the comment, so the row can link
// to it. Other fields keep their meaning.
func TestWakeupTriggerDetailsCopiesCommentID(t *testing.T) {
	receipts := []db.IssueWakeupReceipt{{
		EventType: "comment.created",
		Payload:   json.RawMessage(`{"comment_id":"c-1","actor_type":"member","actor_id":"u-1","coalesced_count":1}`),
	}}
	details := wakeupTriggerDetails(db.AgentTaskQueue{}, receipts)
	if got := details["comment_id"]; got != "c-1" {
		t.Fatalf("comment_id = %v, want c-1 (details %v)", got, details)
	}
	if details["actor_id"] != "u-1" {
		t.Fatalf("actor_id = %v", details["actor_id"])
	}
}

// With several merged comments the row links to the latest one.
func TestWakeupTriggerDetailsKeepsLatestCommentID(t *testing.T) {
	receipts := []db.IssueWakeupReceipt{
		{EventType: "comment.created", Payload: json.RawMessage(`{"comment_id":"c-1"}`)},
		{EventType: "comment.created", Payload: json.RawMessage(`{"comment_id":"c-2"}`)},
	}
	if got := wakeupTriggerDetails(db.AgentTaskQueue{}, receipts)["comment_id"]; got != "c-2" {
		t.Fatalf("comment_id = %v, want c-2", got)
	}
}

// Non-comment triggers add no comment_id.
func TestWakeupTriggerDetailsOmitsCommentIDWithoutOne(t *testing.T) {
	receipts := []db.IssueWakeupReceipt{{EventType: "issue.status_changed", Payload: json.RawMessage(`{"to":"done"}`)}}
	if _, ok := wakeupTriggerDetails(db.AgentTaskQueue{}, receipts)["comment_id"]; ok {
		t.Fatal("comment_id set without a comment")
	}
}
