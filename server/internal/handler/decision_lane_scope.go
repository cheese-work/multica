package handler

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// decisionReaskWindow is how long an unanswered decision stays un-askable again.
const decisionReaskWindow = 24 * time.Hour

var ictZone = time.FixedZone("ICT", 7*60*60)

// requireLaneNamesIssue refuses a DM where the issue was never discussed and
// lists the DMs where it was. Eligibility is read for this one DM, not from the
// ten-lane listing, so an old discussion stays eligible.
func requireLaneNamesIssue(ctx context.Context, queries *db.Queries, issue db.Issue, session db.ChatSession) error {
	lanes := db.ListDecisionLanesParams{WorkspaceID: issue.WorkspaceID, AgentID: session.AgentID, CreatorID: session.CreatorID, IssueID: issue.ID}
	own := lanes
	own.SessionID = session.ID
	rows, err := queries.ListDecisionLanes(ctx, own)
	if err != nil {
		return err
	}
	if len(rows) == 1 && rows[0].ReferencedAt.Valid {
		return nil
	}
	rows, err = queries.ListDecisionLanes(ctx, lanes)
	if err != nil {
		return err
	}
	var matching []string
	for _, lane := range rows {
		if lane.ReferencedAt.Valid {
			matching = append(matching, fmt.Sprintf("%s (%q)", uuidToString(lane.ID), lane.Title))
		}
	}
	hint := "no DM names this issue; use --new-dm"
	if len(matching) > 0 {
		hint = "DMs that name it: " + strings.Join(matching, ", ")
	}
	return decisionIntentRefusal{http.StatusForbidden, "DM " + uuidToString(session.ID) + " never discussed this issue; " + hint}
}

// askSourceTail is the per-run provenance link that ends a rendered ask; a
// fresh source comment must not make the same decision look new.
var askSourceTail = regexp.MustCompile(`\n\n\*\*Source\*\*\n[^\n]*$`)

// requireNoRecentReask refuses the same unanswered ask from the same agent to
// the same person on the same issue within decisionReaskWindow of the earlier
// one. The ask is compared without its source link.
func requireNoRecentReask(ctx context.Context, queries *db.Queries, issue db.Issue, agentID, humanID pgtype.UUID, ask string) error {
	earlier, err := queries.OpenDecisionAsksSince(ctx, db.OpenDecisionAsksSinceParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AskingAgentID: agentID, HumanUserID: humanID})
	if err != nil {
		return err
	}
	identity := askSourceTail.ReplaceAllString(ask, "")
	for _, row := range earlier {
		if askSourceTail.ReplaceAllString(row.Ask, "") != identity {
			continue
		}
		const layout = "2006-01-02 15:04 ICT"
		return decisionIntentRefusal{http.StatusConflict, fmt.Sprintf("this decision was already asked at %s and is unanswered; re-ask allowed after %s", row.CreatedAt.Time.In(ictZone).Format(layout), row.CreatedAt.Time.Add(decisionReaskWindow).In(ictZone).Format(layout))}
	}
	return nil
}
