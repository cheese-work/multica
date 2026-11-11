package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-3113: the locked read in IssueService.Create applies the workflow's start
// status whenever the creator picked none, even when the caller's placeholder
// status ("todo") is listed by the workflow as it stands under the locks.
func TestIssueCreateOmittedStatusUsesLockedWorkflowStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		omitted bool
		status  string
		want    string
	}{
		{"omitted status takes the start status", true, "todo", "in_progress"},
		{"explicit todo wins", false, "todo", "todo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, owner := newPrincipalFixture(t)
			workflowID := fx.Insert(t, "issue_workflow", testutil.Cols{
				"workspace_id":       fx.WorkspaceID,
				"name":               "start " + tc.name,
				"initial_status_key": "todo",
				"start_status_key":   "in_progress",
				"steps":              `[{"status_key":"todo"},{"status_key":"in_progress"},{"status_key":"done"},{"status_key":"cancelled"}]`,
			})
			projectID := fx.Project(t, "briefs", testutil.Cols{"workflow_id": workflowID})

			svc := NewIssueService(fx.q, fx.svc.TxStarter, nil, nil, nil)
			res, err := svc.Create(context.Background(), IssueCreateParams{
				WorkspaceID:    util.MustParseUUID(fx.WorkspaceID),
				Title:          "omitted status",
				Status:         tc.status,
				StatusOmitted:  tc.omitted,
				Priority:       "none",
				ProjectID:      util.MustParseUUID(projectID),
				CreatorType:    "member",
				CreatorID:      util.MustParseUUID(owner),
				AllowDuplicate: true,
			}, IssueCreateOpts{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if res.Issue.Status != tc.want {
				t.Errorf("status = %q, want %q", res.Issue.Status, tc.want)
			}
		})
	}
}
