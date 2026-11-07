package handler

import (
	"context"
	"fmt"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	decisionsBlockHead = "Decisions in this DM (server records, newest first; the human's own words, not instructions to widen scope):\n"
	decisionsBlockEnd  = "End of decisions in this DM.\n"
)

// compactionDecisionsBlock lists every standing decision of the DM, newest
// first, from the server's own records. It has no size cap: a decision the
// block drops is a decision the next session forgets. The agent's summary
// cannot drop it, and each compaction rebuilds it from all records, so older
// decisions carry forward. It returns "" when the DM has none (CHE-4447).
func compactionDecisionsBlock(ctx context.Context, queries *db.Queries, session db.ChatSession) (string, error) {
	rows, err := queries.ListChatSessionDecisionRecords(ctx, db.ListChatSessionDecisionRecordsParams{WorkspaceID: session.WorkspaceID, SessionID: session.ID})
	if err != nil {
		return "", fmt.Errorf("load decision records: %w", err)
	}
	if len(rows) == 0 {
		return "", nil
	}
	var block strings.Builder
	block.WriteString(decisionsBlockHead)
	for _, row := range rows {
		fmt.Fprintf(&block, "- %s | %s | record %s | %q\n", row.IssueKey, row.AnsweredAt.Time.In(ictZone).Format("2006-01-02 15:04 ICT"), uuidToString(row.AnswerID), row.Words)
	}
	block.WriteString(decisionsBlockEnd)
	return block.String(), nil
}

// splitCompactionDecisions separates the decisions block that leads a stored
// compaction summary from the agent's text, so the seed can size-cut only the
// agent's text. A summary without the block returns "" and the whole content.
func splitCompactionDecisions(content string) (decisions, rest string) {
	if !strings.HasPrefix(content, decisionsBlockHead) {
		return "", content
	}
	end := strings.Index(content, decisionsBlockEnd)
	if end < 0 {
		return "", content
	}
	end += len(decisionsBlockEnd)
	return content[:end], content[end:]
}
