package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-4549 (S2 of CHE-4439): a member's status or assignee change marks the
// issue's latest route label overridden, under the issue lock.

func routeReversed(t *testing.T, issueID string) []bool {
	t.Helper()
	var out []bool
	dbfx.QueryRow(t, `SELECT COALESCE(array_agg(reversed_at IS NOT NULL ORDER BY created_at, id), '{}') FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, issueID).Scan(&out)
	return out
}

func wantReversed(t *testing.T, issueID string, want ...bool) {
	t.Helper()
	got := routeReversed(t, issueID)
	if len(got) != len(want) {
		t.Fatalf("route rows reversed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("route rows reversed = %v, want %v", got, want)
		}
	}
}

func (c routeWriteCase) memberWrite(t *testing.T, mutate func(*db.UpdateIssueParams)) error {
	t.Helper()
	params := db.UpdateIssueParams{ID: parseUUID(c.issueID)}
	mutate(&params)
	_, err := testHandler.updateIssueWithStatusGuard(withMemberRouteOverride(context.Background()), parseUUID(testWorkspaceID), "", params, nil)
	return err
}

func memberStatus(to string) func(*db.UpdateIssueParams) {
	return func(p *db.UpdateIssueParams) { p.Status = pgtype.Text{String: to, Valid: true} }
}

func TestMemberWriteOverridesTheLatestRoute(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	wantReversed(t, c.issueID, false)
	if err := c.memberWrite(t, memberStatus("in_review")); err != nil {
		t.Fatal(err)
	}
	wantReversed(t, c.issueID, true)
}

func TestMemberAssigneeChangeOverridesTheLatestRoute(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	err := c.memberWrite(t, func(p *db.UpdateIssueParams) {
		p.AssigneeType = pgtype.Text{String: "member", Valid: true}
		p.AssigneeID = parseUUID(testUserID)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantReversed(t, c.issueID, true)
}

func TestMemberWriteWithoutStatusOrAssigneeChangeKeepsTheRoute(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	title := func(p *db.UpdateIssueParams) { p.Title = pgtype.Text{String: "renamed", Valid: true} }
	if err := c.memberWrite(t, title); err != nil {
		t.Fatal(err)
	}
	// Same status again is no change either.
	if err := c.memberWrite(t, memberStatus("in_progress")); err != nil {
		t.Fatal(err)
	}
	wantReversed(t, c.issueID, false)
}

func TestUnarmedWriteKeepsTheRoute(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	params := db.UpdateIssueParams{ID: parseUUID(c.issueID), Status: pgtype.Text{String: "in_review", Valid: true}}
	if _, err := testHandler.updateIssueWithStatusGuard(context.Background(), parseUUID(testWorkspaceID), "", params, nil); err != nil {
		t.Fatal(err)
	}
	wantReversed(t, c.issueID, false)
}

func TestMemberWriteMarksOnlyTheLatestRoute(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	c.hook.used, c.rec.eventID = false, pgtype.UUID{}
	c.rec.activity = nil
	if _, err := c.write(t, "in_review", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.memberWrite(t, memberStatus("done")); err != nil {
		t.Fatal(err)
	}
	wantReversed(t, c.issueID, false, true)
}

func TestMemberWriteSkipsARouteOlderThanAnHour(t *testing.T) {
	for name, tc := range map[string]struct {
		age  time.Duration
		want bool
	}{"59 minutes": {59 * time.Minute, true}, "61 minutes": {61 * time.Minute, false}} {
		t.Run(name, func(t *testing.T) {
			c := newRouteWriteCase(t)
			if _, err := c.write(t, "in_progress", nil); err != nil {
				t.Fatal(err)
			}
			dbfx.Exec(t, `UPDATE system1_observation SET created_at = clock_timestamp() - make_interval(secs => $2) WHERE kind = 'route' AND subject_id = $1`, c.issueID, tc.age.Seconds())
			if err := c.memberWrite(t, memberStatus("in_review")); err != nil {
				t.Fatal(err)
			}
			wantReversed(t, c.issueID, tc.want)
		})
	}
}

func TestMemberWriteSkipsAnAlreadyReversedRoute(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.memberWrite(t, memberStatus("in_review")); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	dbfx.QueryRow(t, `SELECT reversed_at FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID).Scan(&first)
	if err := c.memberWrite(t, memberStatus("done")); err != nil {
		t.Fatal(err)
	}
	var second time.Time
	dbfx.QueryRow(t, `SELECT reversed_at FROM system1_observation WHERE kind = 'route' AND subject_id = $1`, c.issueID).Scan(&second)
	if !first.Equal(second) {
		t.Errorf("reversed_at moved from %v to %v", first, second)
	}
}

func TestMemberWriteDoesNotReverseItsOwnRoute(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	// The member write carries its own route capture: prior reversed, own not.
	c.hook.used, c.hook.DecisionSource, c.hook.Caller = false, "human", workflowCaller{}
	ctx := withMemberRouteOverride(withRouteCapture(withStatusRecord(context.Background(), c.rec), c.hook))
	params := db.UpdateIssueParams{ID: parseUUID(c.issueID), Status: pgtype.Text{String: "in_review", Valid: true}}
	if _, err := testHandler.updateIssueWithStatusGuard(ctx, parseUUID(testWorkspaceID), "", params, nil); err != nil {
		t.Fatal(err)
	}
	wantReversed(t, c.issueID, true, false)
}

func TestMemberOverrideRollsBackWithAFailedWrite(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	c.hook.used, c.hook.DecisionSource = false, "bogus" // the capture CHECK fails after the mark
	ctx := withMemberRouteOverride(withRouteCapture(withStatusRecord(context.Background(), c.rec), c.hook))
	params := db.UpdateIssueParams{ID: parseUUID(c.issueID), Status: pgtype.Text{String: "in_review", Valid: true}}
	if _, err := testHandler.updateIssueWithStatusGuard(ctx, parseUUID(testWorkspaceID), "", params, nil); err == nil {
		t.Fatal("capture failure must fail the write")
	}
	wantReversed(t, c.issueID, false)
}

func TestMemberOverrideOnTheAtomicBranch(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	params := db.UpdateIssueParams{ID: parseUUID(c.issueID), Status: pgtype.Text{String: "in_review", Valid: true}, Description: pgtype.Text{String: "d", Valid: true}}
	if _, _, _, err := testHandler.updateIssueAtomically(withMemberRouteOverride(context.Background()), parseUUID(testWorkspaceID), params, nil, nil, nil, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	wantReversed(t, c.issueID, true)
}

// The UpdateIssue handler arms the override for a member only.
func TestUpdateIssueArmsTheOverrideForMembersOnly(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	patch := func(status string) {
		req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+c.issueID, map[string]any{"status": status, "suppress_run": true}), "id", c.issueID)
		req.Header.Set("X-Actor-Source", "interactive_jwt")
		testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	}
	patch("in_review")
	wantReversed(t, c.issueID, true)
}

// Terra's r3 note: the mark binds to the route that existed under the issue
// lock. A route that commits after the member write is not the one overridden.
func TestMemberOverrideBindsToThePriorRouteUnderTheLock(t *testing.T) {
	c := newRouteWriteCase(t)
	if _, err := c.write(t, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	guard := func(ctx context.Context, q *db.Queries, params *db.UpdateIssueParams) error {
		if _, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: params.ID, WorkspaceID: parseUUID(testWorkspaceID)}); err != nil {
			return err
		}
		close(locked)
		<-release
		return nil
	}
	memberDone := make(chan error, 1)
	go func() {
		ctx := withMemberRouteOverride(withIssueMoveGuard(context.Background(), guard))
		params := db.UpdateIssueParams{ID: parseUUID(c.issueID), Status: pgtype.Text{String: "in_review", Valid: true}}
		_, err := testHandler.updateIssueWithStatusGuard(ctx, parseUUID(testWorkspaceID), "", params, nil)
		memberDone <- err
	}()
	<-locked

	// A later agent route tries to commit while the member write holds the lock.
	c.hook.used, c.rec.eventID = false, pgtype.UUID{}
	c.rec.activity = nil
	agentDone := make(chan error, 1)
	go func() {
		_, err := c.write(t, "done", nil)
		agentDone <- err
	}()
	select {
	case err := <-agentDone:
		t.Fatalf("agent route committed while the member write held the issue lock (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-memberDone; err != nil {
		t.Fatal(err)
	}
	if err := <-agentDone; err != nil {
		t.Fatal(err)
	}
	wantReversed(t, c.issueID, true, false)
}
