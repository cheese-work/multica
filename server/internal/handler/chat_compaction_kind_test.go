package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-4438: the web client folds the summary and repeats its questions only
// when it sees the compaction_summary kind, so both the stored-message API and
// the live event must keep it.
func TestStoredCompactionSummaryKeepsItsKindInTheAPI(t *testing.T) {
	got := chatMessageToResponse(db.ChatMessage{MessageKind: protocol.ChatMessageKindCompactionSummary}, nil).MessageKind
	if got != protocol.ChatMessageKindCompactionSummary {
		t.Fatalf("API message_kind = %q, want %q", got, protocol.ChatMessageKindCompactionSummary)
	}
}

func TestCompactionSummaryLiveEventKeepsItsKind(t *testing.T) {
	payload := compactionSummaryPayload("session", db.ChatMessage{Role: "assistant", Content: "Goal."}, "task")
	if payload.MessageKind != protocol.ChatMessageKindCompactionSummary {
		t.Fatalf("live event message_kind = %q, want %q", payload.MessageKind, protocol.ChatMessageKindCompactionSummary)
	}
	if payload.ChatSessionID != "session" || payload.Content != "Goal." || payload.TaskID != "task" {
		t.Fatalf("payload lost its fields: %+v", payload)
	}
}
