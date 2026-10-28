package handler

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// missingWebhookEndpoint returns the save error for the first webhook(name)
// edge without an enabled endpoint of that name on the workflow, or "" when
// every edge has one. A workflow that is not stored yet (invalid workflowID)
// has no endpoints, so it cannot carry the edge.
func missingWebhookEndpoint(ctx context.Context, q *db.Queries, workspaceID, workflowID pgtype.UUID, steps []issueworkflow.Step) (string, error) {
	enabled := map[string]bool{}
	if workflowID.Valid {
		endpoints, err := q.ListWorkflowWebhookEndpoints(ctx, db.ListWorkflowWebhookEndpointsParams{WorkspaceID: workspaceID, WorkflowID: workflowID})
		if err != nil {
			return "", err
		}
		for _, ep := range endpoints {
			if !ep.DisabledAt.Valid {
				enabled[ep.Name] = true
			}
		}
	}
	for _, step := range steps {
		for _, edge := range step.Edges {
			guard, err := issueworkflow.ParseGuard(edge.Guard)
			if err != nil || guard.Kind != issueworkflow.GuardWebhook || enabled[guard.Args[0]] {
				continue
			}
			return fmt.Sprintf("step %q: no enabled webhook endpoint %q on this workflow; create it first with `multica workflow webhook create`", step.StatusKey, guard.Args[0]), nil
		}
	}
	return "", nil
}
