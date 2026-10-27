package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// decisionReaskWindow is how long an unanswered decision stays un-askable again.
const decisionReaskWindow = 24 * time.Hour

var ictZone = time.FixedZone("ICT", 7*60*60)

// decisionLaneReferences maps each lane to the newest time it named the issue
// (a message mention or a decision request on it); absent lanes never did.
func decisionLaneReferences(ctx context.Context, queries *db.Queries, issue db.Issue, lanes []db.ListDecisionLanesRow) (map[pgtype.UUID]time.Time, error) {
	ids := make([]pgtype.UUID, len(lanes))
	for index, lane := range lanes {
		ids[index] = lane.ID
	}
	rows, err := queries.ListDecisionLaneIssueReferences(ctx, db.ListDecisionLaneIssueReferencesParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, SessionIds: ids})
	if err != nil {
		return nil, err
	}
	references := make(map[pgtype.UUID]time.Time, len(rows))
	for _, row := range rows {
		references[row.ChatSessionID] = row.ReferencedAt.Time
	}
	return references, nil
}

// requireLaneNamesIssue refuses a DM where the issue was never discussed and
// lists the DMs where it was.
func requireLaneNamesIssue(ctx context.Context, queries *db.Queries, issue db.Issue, session db.ChatSession) error {
	lanes, err := queries.ListDecisionLanes(ctx, db.ListDecisionLanesParams{WorkspaceID: issue.WorkspaceID, AgentID: session.AgentID, CreatorID: session.CreatorID})
	if err != nil {
		return err
	}
	references, err := decisionLaneReferences(ctx, queries, issue, lanes)
	if err != nil {
		return err
	}
	if _, named := references[session.ID]; named {
		return nil
	}
	var matching []string
	for _, lane := range lanes {
		if _, named := references[lane.ID]; named {
			matching = append(matching, fmt.Sprintf("%s (%q)", uuidToString(lane.ID), lane.Title))
		}
	}
	hint := "no DM names this issue; use --new-dm"
	if len(matching) > 0 {
		hint = "DMs that name it: " + strings.Join(matching, ", ")
	}
	return decisionIntentRefusal{http.StatusForbidden, "DM " + uuidToString(session.ID) + " never discussed this issue; " + hint}
}

// requireNoRecentReask refuses the same unanswered ask from the same agent to
// the same person on the same issue within decisionReaskWindow of the earlier one.
func requireNoRecentReask(ctx context.Context, queries *db.Queries, issue db.Issue, agentID, humanID pgtype.UUID, ask string) error {
	earlier, err := queries.LatestOpenDecisionAsk(ctx, db.LatestOpenDecisionAskParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AskingAgentID: agentID, HumanUserID: humanID, Ask: ask})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	const layout = "2006-01-02 15:04 ICT"
	return decisionIntentRefusal{http.StatusConflict, fmt.Sprintf("this decision was already asked at %s and is unanswered; re-ask allowed after %s", earlier.Time.In(ictZone).Format(layout), earlier.Time.Add(decisionReaskWindow).In(ictZone).Format(layout))}
}
