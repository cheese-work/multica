package handler

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// When no card can open (a workspace with nobody to ask), the issue says so in
// a system comment: need_decision never lacks its card silently.
func TestRoundCap_MissingCardIsAnnouncedOnTheIssue(t *testing.T) {
	suffix := workflowTestSuffix()
	wsID := dbfx.Workspace(t, "Empty "+suffix, "empty-"+suffix)
	issueID := dbfx.Issue(t, "no one to ask", testutil.Cols{"workspace_id": wsID})
	issue, err := testHandler.Queries.GetIssue(t.Context(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.ensureReviewRoundsCard(t.Context(), issue, roundsCap{rounds: 3})
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'system' AND content LIKE '%could not open its decision card%'`, issueID); n != 1 {
		t.Fatalf("missing-card notices = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("decision requests = %d, want 0", n)
	}
}

// CHE-9483, CHE-9511: an issue of any type that reaches the round limit gets no
// decision card when the setting is on. The server records the auto-Split as an
// agent decision and tells the need_decision handler to run the Split path.
func TestRoundCap_AnyIssueTypeAutoSplitsWithoutCard(t *testing.T) {
	enableRoundsAutoSplit(t, testWorkspaceID)
	for _, title := range []string{"plan(WE006): split the workstream", "fix(handler): split the fix", "task: split the task", "no type prefix"} {
		t.Run(title, func(t *testing.T) { wantAutoSplitWithoutCard(t, title) })
	}
}

func wantAutoSplitWithoutCard(t *testing.T, title string) {
	t.Helper()
	issueID := dbfx.Issue(t, title, testutil.Cols{})
	issue, err := testHandler.Queries.GetIssue(t.Context(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	var capped roundsCap
	if err := testHandler.withPgxTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		capped, err = recordRoundsCap(t.Context(), testHandler.Queries.WithTx(tx), issue, pgtype.UUID{}, 3)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !capped.autoSplit {
		t.Fatal("cap did not take the auto-Split path")
	}
	testHandler.ensureReviewRoundsCard(t.Context(), issue, capped)
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("decision requests = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'review_rounds_auto_split' AND details->>'rounds' = '3'`, issueID); n != 1 {
		t.Fatalf("auto-split activity rows = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'system' AND content LIKE '%auto-Split at round limit%' AND content LIKE '%3 review rounds%'`, issueID); n != 1 {
		t.Fatalf("auto-split notices = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%could not open its decision card%'`, issueID); n != 0 {
		t.Fatalf("missing-card notices = %d, want 0", n)
	}
}

// enableRoundsAutoSplit turns the workspace setting on for one test.
func enableRoundsAutoSplit(t *testing.T, wsID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE workspace SET settings = coalesce(settings, '{}'::jsonb) || '{"review_rounds_auto_split":"on"}'::jsonb WHERE id = $1`, wsID)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'review_rounds_auto_split' WHERE id = $1`, wsID)
	})
}

func TestRoundCap_AutoSplitSettingValidation(t *testing.T) {
	for value, wantErr := range map[any]bool{"on": false, "off": false, "yes": true, nil: true, true: true} {
		err := validateRoundsAutoSplitSetting(map[string]any{"review_rounds_auto_split": value})
		if (err != nil) != wantErr {
			t.Errorf("value %v: err = %v, want error %v", value, err, wantErr)
		}
	}
	if err := validateRoundsAutoSplitSetting(map[string]any{}); err != nil {
		t.Errorf("absent key: %v", err)
	}
}
