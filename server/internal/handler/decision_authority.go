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
	return requireDecisionAuthorityIn(ctx, queries, issue, humanID, class, pgtype.UUID{}, false, status)
}

// requireRequestAuthority applies the rule to an existing request. A request
// made in another project than the issue's current one no longer carries
// delegate authority, even when the person is also a delegate of the new
// project: the lifecycle worker cancels it, but record and delivery must not
// act on it before that runs. A server-opened request (CHE-2318) was addressed
// to its person by the platform, not by an agent's ask, so it needs only an
// active membership: its options move no authority, the person acts on the
// issue themself.
func requireRequestAuthority(ctx context.Context, queries *db.Queries, issue db.Issue, request db.DecisionRequest, status int) error {
	return requireDecisionAuthorityIn(ctx, queries, issue, request.HumanUserID, request.EscalationClass, request.ProjectID, isServerDecision(request), status)
}

func requireDecisionAuthorityIn(ctx context.Context, queries *db.Queries, issue db.Issue, humanID pgtype.UUID, class string, recordedProject pgtype.UUID, serverOpened bool, status int) error {
	authority, err := queries.DecisionHumanAuthority(ctx, db.DecisionHumanAuthorityParams{WorkspaceID: issue.WorkspaceID, UserID: humanID, ProjectID: issue.ProjectID})
	if errors.Is(err, pgx.ErrNoRows) {
		return decisionIntentRefusal{http.StatusForbidden, "decision person is not an active workspace member"}
	}
	if err != nil {
		return err
	}
	moved := recordedProject.Valid && recordedProject != issue.ProjectID
	if authority.IsOwner || serverOpened || class == "reversible" && authority.IsDelegate && !moved {
		return nil
	}
	return decisionIntentRefusal{status, decisionClassNeedsOwner + ": this decision concerns the workspace owner; post a fresh --decision-intent that names the owner"}
}
