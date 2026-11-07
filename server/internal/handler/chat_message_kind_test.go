package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestNormalizeMessageKind_KeepsCompactionSummary(t *testing.T) {
	if got := normalizeMessageKind(protocol.ChatMessageKindCompactionSummary); got != protocol.ChatMessageKindCompactionSummary {
		t.Fatalf("normalizeMessageKind() = %q, want %q", got, protocol.ChatMessageKindCompactionSummary)
	}
}
