package handler

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// windowOpened reports whether a person's row on the issue opens a run-budget
// window (CHE-2798 S1d).
func windowOpened(t *testing.T, issueID string) bool {
	t.Helper()
	_, err := testHandler.Queries.LatestMemberStatusChange(context.Background(), parseUUID(issueID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return err == nil
}

func patchStatusAs(issueID, status, source string) *http.Request {
	req := patchStatus(issueID, status)
	req.Header.Set("X-Actor-Source", source)
	return req
}

// A status move opens a window only when an interactive session made it.
func TestStatusMoveOpensWindowOnlyForInteractiveJWT(t *testing.T) {
	for source, want := range map[string]bool{
		"interactive_jwt": true, "cli_jwt": false, "pat": false, "cloud_pat": false, "task_token": false,
	} {
		issueID := createIssueForTimeline(t, "window "+source)
		testutil.Call(t, testHandler.UpdateIssue, patchStatusAs(issueID, "in_progress", source)).Want(http.StatusOK)
		if got := windowOpened(t, issueID); got != want {
			t.Errorf("%s status move opened a window = %v, want %v", source, got, want)
		}
	}
}

// Rows no interactive request wrote never open a window after the cutover,
// whatever the actor type says; a row before it keeps today's rule.
func TestWindowRowsAfterCutover(t *testing.T) {
	const pastCutover = "2020-01-01T00:00:00Z"
	cases := []struct {
		name, action, details, at string
		want                      bool
	}{
		{"interactive ui_or_api", "status_changed", `{"trigger":{"cause":{"kind":"ui_or_api"},"auth_source":"interactive_jwt"}}`, "", true},
		{"interactive remap", "status_changed", `{"trigger":{"cause":{"kind":"workflow_remap"},"auth_source":"interactive_jwt"}}`, "", false},
		{"interactive other cause", "status_changed", `{"trigger":{"cause":{"kind":"workflow_edge"},"auth_source":"interactive_jwt"}}`, "", false},
		{"cli_jwt", "status_changed", `{"trigger":{"cause":{"kind":"ui_or_api"},"auth_source":"cli_jwt"}}`, "", false},
		{"no trigger", "status_changed", `{}`, "", false},
		{"top-up", "run_budget_top_up", `{}`, "", true},
		{"before cutover, no trigger", "status_changed", `{}`, pastCutover, true},
		{"before cutover, remap", "status_changed", `{"trigger":{"cause":{"kind":"workflow_remap"}}}`, pastCutover, false},
	}
	for _, c := range cases {
		issueID := createIssueForTimeline(t, "window rows "+c.name)
		dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
			VALUES ($1, $2, 'member', $3, $4, $5::jsonb, COALESCE(NULLIF($6, '')::timestamptz, now()))`,
			testWorkspaceID, issueID, testUserID, c.action, c.details, c.at)
		if got := windowOpened(t, issueID); got != c.want {
			t.Errorf("%s: opened a window = %v, want %v", c.name, got, c.want)
		}
	}
}

// The migration records the cutover once.
func TestRunBudgetCutoverRecordedOnce(t *testing.T) {
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM run_budget_window_cutover`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("cutover rows = %d (err %v), want 1", n, err)
	}
}

// A refused dispatch resumes on any person's status move, so candidate
// selection and delivery must agree: a PAT move selects the episode and
// resumes it; a remap row does neither. A selected episode that delivery
// leaves suspended would hold a batch slot for every tenant (CWE-400).
func TestRefusedDispatchSelectionMatchesDelivery(t *testing.T) {
	for name, tc := range map[string]struct {
		trigger string
		resumes bool
	}{
		"pat move":   {`{"trigger":{"cause":{"kind":"ui_or_api"},"auth_source":"pat"}}`, true},
		"remap move": {`{"trigger":{"cause":{"kind":"workflow_remap"},"auth_source":"interactive_jwt"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEpisodeFixture(t, false)
			f.enter(t)
			f.answerRequest(t, f.requestID, -time.Minute, false)
			f.conflictingTask(t)
			f.deliverEpisodeOnly(t)
			dbfx.Exec(t, `UPDATE decision_wait SET dispatch_refused_since = now() - interval '16 minutes' WHERE issue_id = $1 AND closed_at IS NULL`, f.issueID)
			f.deliverEpisodeOnly(t)
			before := f.suspension(t)
			if before.phase != "suspended" || before.reason != "dispatch_refused" {
				t.Fatalf("episode = %+v, want suspended dispatch_refused", before)
			}
			dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
				VALUES ($1, $2, 'member', $3, 'status_changed', $4::jsonb, now() + interval '1 second')`, testWorkspaceID, f.issueID, testUserID, tc.trigger)

			waitID := f.onlyEpisode(t).id
			selected := slices.Contains(episodeCandidateIDs(t, 0), waitID)
			if selected != tc.resumes {
				t.Fatalf("selected = %v, want %v", selected, tc.resumes)
			}
			if !selected {
				return
			}
			// The refused task is still active, so a resumed episode is refused
			// again and suspends again: a later suspended_at proves delivery let it
			// resume instead of holding it.
			f.deliverEpisodeOnly(t)
			if after := f.suspension(t); !after.suspendedAt.After(before.suspendedAt) {
				t.Fatalf("episode = %+v: selected but delivery held it suspended", after)
			}
		})
	}
}
