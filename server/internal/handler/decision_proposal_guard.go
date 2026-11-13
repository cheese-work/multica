package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// proposalRejectionCooldown is how long a rejected action stays refused.
	proposalRejectionCooldown = time.Hour
	// maxProposalReasonBytes caps the agent's unverified note.
	maxProposalReasonBytes = 1024
)

// admitProposalOnce is the guard New wires. There is no cap on open proposals
// (Cheese, CHE-2949); it refuses a rejected action inside the cooldown and a
// re-ask of the same unanswered ask.
func admitProposalOnce(ctx context.Context, queries *db.Queries, proposal ProposalAdmission) error {
	if err := requireNoRecentRejection(ctx, queries, proposal); err != nil {
		return err
	}
	return requireNoRecentReask(ctx, queries, proposal.Issue, proposal.Agent.ID, proposal.RecipientID, proposal.Ask)
}

// requireNoRecentRejection refuses an action this person rejected within the
// cooldown. The key is the proposal identity without the lane, so another lane
// does not reset it. Stored params are JSONB, so each is re-canonicalized
// before its hash is compared.
func requireNoRecentRejection(ctx context.Context, queries *db.Queries, proposal ProposalAdmission) error {
	rows, err := queries.ListRecentRejectedProposalParams(ctx, db.ListRecentRejectedProposalParamsParams{
		WorkspaceID: proposal.Issue.WorkspaceID, AgentID: proposal.Agent.ID, RecipientUserID: proposal.RecipientID,
		IssueID: proposal.Issue.ID, ActionType: proposal.ActionType, ActionVersion: int32(proposal.ActionVersion),
		Cooldown: pgtype.Interval{Microseconds: proposalRejectionCooldown.Microseconds(), Valid: true},
	})
	if err != nil {
		return err
	}
	for _, row := range rows {
		canonical, err := decisionaction.Canonical(row.Params)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(canonical)
		if hex.EncodeToString(sum[:]) != proposal.ParamsSHA256 {
			continue
		}
		const layout = "2006-01-02 15:04 ICT"
		return decisionIntentRefusal{http.StatusTooManyRequests, fmt.Sprintf("proposal_cooldown: this action was rejected at %s; it can be proposed again after %s", row.UpdatedAt.Time.In(ictZone).Format(layout), row.UpdatedAt.Time.Add(proposalRejectionCooldown).In(ictZone).Format(layout))}
	}
	return nil
}

// cleanProposalReason drops control and format characters (escape codes, NUL,
// bidi overrides) and invalid UTF-8, keeping newlines. The note is shown as a
// quote and never reaches a binding fact.
func cleanProposalReason(reason string) string {
	var out strings.Builder
	for _, char := range reason {
		if char == '\n' || (char != utf8.RuneError && !unicode.IsControl(char) && !unicode.Is(unicode.Cf, char)) {
			out.WriteRune(char)
		}
	}
	return out.String()
}
