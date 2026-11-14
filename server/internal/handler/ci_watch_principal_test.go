package handler

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/ciwatch"
)

// sameHolderClaims grants every claim, as a free red-main check would.
type sameHolderClaims struct{}

func (sameHolderClaims) ClaimCheck(context.Context, pgx.Tx, string, string, string, string) (bool, error) {
	return true, nil
}

func TestCIWatchDestinationsAreShadowOnlyForRedMain(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	if testHandler.CIWatchDestinations().RedMain != nil {
		t.Fatal("RedMain is wired without a real GitHub ref claimer; issue creation must stay Shadow-only")
	}
}

// CHE-4279 review round 2: the wake is checked against the principal's current
// invocation permissions. A denied wake leaves the issue and starts no run.
func TestCIWatchWakeChecksThePrincipalsInvocationPermission(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	dest := testHandler.CIWatchDestinations().Issue
	var plain string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('CI Watch Plain', 'ciwatch-plain@multica.test') RETURNING id`).Scan(&plain); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE email = 'ciwatch-plain@multica.test'`)
	})
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1,$2,'member')`, testWorkspaceID, plain); err != nil {
		t.Fatal(err)
	}
	const outsider = "00000000-0000-4000-8000-00000000c1c1"

	type setup func(agent string)
	revoke := func(agent string) {
		testPool.Exec(ctx, `DELETE FROM agent_invocation_target WHERE agent_id = $1`, agent)
	}
	archive := func(agent string) { testPool.Exec(ctx, `UPDATE agent SET archived_at = now() WHERE id = $1`, agent) }
	none := func(string) {}
	cases := []struct {
		name      string
		principal string
		prep      setup
		squad     bool
		wantTasks int
	}{
		{"allowed workspace member", testUserID, none, false, 1},
		{"member of a public_to workspace agent", plain, none, false, 1},
		{"grant revoked", plain, revoke, false, 0},
		{"archived agent", testUserID, archive, false, 0},
		{"principal outside the workspace", outsider, none, false, 0},
		{"squad leader revoked", plain, revoke, true, 0},
		{"squad leader allowed", plain, none, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agent := createHandlerTestAgent(t, "CI Principal "+c.name, nil)
			c.prep(agent)
			spec := ciwatch.IssueSpec{WorkspaceID: testWorkspaceID, Title: "fix(ci): principal", Body: "b", Priority: "urgent",
				AssigneeType: "agent", AssigneeID: agent, CreatorID: c.principal, Wake: true}
			if c.squad {
				spec.AssigneeType, spec.AssigneeID = "squad", insertSquad(t, ctx, testWorkspaceID, agent, "CI Principal Squad "+agent[:8])
			}
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			id, err := dest.CreateIssue(ctx, tx, spec)
			if err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, id)
				testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id)
			})
			var issues, tasks int
			testPool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE id = $1`, id).Scan(&issues)
			testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, id).Scan(&tasks)
			if issues != 1 || tasks != c.wantTasks {
				t.Fatalf("issues=%d tasks=%d, want 1/%d", issues, tasks, c.wantTasks)
			}
		})
	}
}

// CHE-4279 review round 3: the member the watcher acts as is the member the task is
// attributed to. For an issue created by someone else, the originator is the
// principal, not the issue creator.
func TestCIWatchWakeAttributesTheTaskToThePrincipalNotTheIssueCreator(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	dest := testHandler.CIWatchDestinations().Issue
	var principal string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('CI Watch Principal', 'ciwatch-principal@multica.test') RETURNING id`).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE email = 'ciwatch-principal@multica.test'`)
	})
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1,$2,'member')`, testWorkspaceID, principal); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"agent", "squad"} {
		t.Run(kind, func(t *testing.T) {
			agent := createHandlerTestAgent(t, "CI Attribution "+kind, nil)
			issueID := createTestIssue(t, "adopted issue "+kind, "todo", "medium") // creator: testUserID
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
				deleteTestIssue(t, issueID)
			})
			assignee := agent
			if kind == "squad" {
				assignee = insertSquad(t, ctx, testWorkspaceID, agent, "CI Attribution Squad "+agent[:8])
			}
			if _, err := testPool.Exec(ctx, `UPDATE issue SET assignee_type=$2, assignee_id=$3 WHERE id=$1`, issueID, kind, assignee); err != nil {
				t.Fatal(err)
			}
			tx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := dest.UpdateIssue(ctx, tx, ciwatch.IssueUpdate{WorkspaceID: testWorkspaceID, IssueID: issueID,
				Priority: "urgent", Wake: true, PrincipalID: principal}); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var originator, accountable string
			if err := testPool.QueryRow(ctx, `SELECT COALESCE(originator_user_id::text,''), COALESCE(accountable_user_id::text,'')
				FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&originator, &accountable); err != nil {
				t.Fatalf("task for the woken owner: %v", err)
			}
			if originator != principal || accountable != principal {
				t.Fatalf("originator=%s accountable=%s, want both %s (issue creator is %s)", originator, accountable, principal, testUserID)
			}
		})
	}
}
