package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const decisionClassNeedsOwner = decisiontx.ClassNeedsOwner

// requireDecisionAuthority allows a decision that concerns humanID only when
// humanID is a workspace owner, or the class is reversible and humanID is a
// decision delegate of the issue's project. It adds to the member,
// canInvokeAgent, window and attestation checks; it replaces none. A person who
// is not a member has no authority.
func requireDecisionAuthority(ctx context.Context, queries *db.Queries, issue db.Issue, humanID pgtype.UUID, class string, status int) error {
	authority, err := queries.DecisionHumanAuthority(ctx, db.DecisionHumanAuthorityParams{WorkspaceID: issue.WorkspaceID, UserID: humanID, ProjectID: issue.ProjectID})
	if errors.Is(err, pgx.ErrNoRows) {
		return decisionIntentRefusal{http.StatusForbidden, "decision person is not an active workspace member"}
	}
	if err != nil {
		return err
	}
	if authority.IsOwner || class == "reversible" && authority.IsDelegate {
		return nil
	}
	return decisionIntentRefusal{status, decisionClassNeedsOwner + ": this decision concerns the workspace owner; post a fresh --decision-intent that names the owner"}
}
