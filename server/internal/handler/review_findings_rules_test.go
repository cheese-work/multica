package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9307: a finding carries a category, and from round 2 on a verdict may add
// a blocker only when the revision introduced it.

func categorized(severity, category, summary string, extra ...map[string]any) map[string]any {
	f := map[string]any{"severity": severity, "summary": summary, "category": category}
	for _, e := range extra {
		for k, v := range e {
			f[k] = v
		}
	}
	return f
}

func setCategoryMode(t *testing.T, mode string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_set(settings, '{review_findings_category}', jsonb_build_object('mode', $2::text)) WHERE id = $1`, testWorkspaceID, mode)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'review_findings_category' WHERE id = $1`, testWorkspaceID)
	})
}

func TestReviewFindings_CategoryIsStoredAndMalformedOneIsRefused(t *testing.T) {
	l := newReviewLoop(t)
	body := l.verdict(t, "in_progress", categorized("P1", "Missing Rollback!", "no rollback")).Want(http.StatusBadRequest).Map()
	if body["code"] != "review_finding_category_required" {
		t.Fatalf("refusal = %v", body)
	}
	l.verdict(t, "in_progress", categorized("P1", "missing_rollback", "no rollback")).Want(http.StatusOK)
	var category string
	dbfx.QueryRow(t, `SELECT (details->>'findings')::jsonb->0->>'category' FROM activity_log WHERE issue_id = $1 AND action = 'review_changes_requested'`, l.issueID).Scan(&category)
	if category != "missing_rollback" {
		t.Fatalf("stored category = %q, want missing_rollback", category)
	}
}

// Rollout: reviewers whose step text still sends `--finding "P1:<summary>"` (no
// category) keep handing changes back after the server deploys.
func TestReviewFindings_FindingWithoutCategoryIsAcceptedUntilRequired(t *testing.T) {
	l := newReviewLoop(t)
	l.verdict(t, "in_progress", map[string]any{"severity": "P1", "summary": "no rollback"}).Want(http.StatusOK)
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status == "agent_review" {
		t.Fatalf("legacy verdict left the issue in agent_review")
	}
}

func TestReviewFindings_RequiredModeRefusesFindingWithoutCategory(t *testing.T) {
	setCategoryMode(t, "required")
	l := newReviewLoop(t)
	body := l.verdict(t, "in_progress", map[string]any{"severity": "P1", "summary": "no rollback"}).Want(http.StatusBadRequest).Map()
	if body["code"] != "review_finding_category_required" {
		t.Fatalf("refusal = %v", body)
	}
	l.verdict(t, "in_progress", categorized("P1", "missing_rollback", "no rollback")).Want(http.StatusOK)
}

func TestValidateReviewFindingsCategorySetting(t *testing.T) {
	for name, tc := range map[string]struct {
		in      any
		wantErr bool
	}{
		"absent":   {map[string]any{}, false},
		"optional": {map[string]any{"review_findings_category": map[string]any{"mode": "optional"}}, false},
		"required": {map[string]any{"review_findings_category": map[string]any{"mode": "required"}}, false},
		"bad mode": {map[string]any{"review_findings_category": map[string]any{"mode": "block"}}, true},
		"bad key":  {map[string]any{"review_findings_category": map[string]any{"x": "required"}}, true},
		"not map":  {map[string]any{"review_findings_category": "required"}, true},
	} {
		if err := validateReviewFindingsCategorySetting(tc.in); (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", name, err, tc.wantErr)
		}
	}
}

// The failing case of CHE-9307: round 2 adds a blocker the revision did not
// introduce. Before the rule it was accepted; now it is refused with the rule text.
func TestReviewFindings_RoundTwoNewBlockerNotFromRevisionIsRefused(t *testing.T) {
	l := newReviewLoop(t)
	l.verdict(t, "in_progress", categorized("P1", "missing_rollback", "no rollback step")).Want(http.StatusOK)
	l.backToReview(t)

	body := l.verdict(t, "in_progress",
		categorized("P1", "missing_rollback", "no rollback step"),
		categorized("P1", "scope_creep", "adds an unrelated endpoint"),
	).Want(http.StatusBadRequest).Map()
	if body["code"] != "review_round_new_blocker" {
		t.Fatalf("refusal = %v", body)
	}
	if msg, _ := body["error"].(string); msg == "" || !strings.Contains(msg, "revision introduced") {
		t.Fatalf("refusal lacks the rule text: %v", body["error"])
	}
	if status, _, _ := issueStatusAndAssignee(t, l.issueID); status != "agent_review" {
		t.Fatalf("status after refused verdict = %q, want agent_review", status)
	}
}

func TestReviewFindings_RoundTwoAcceptsCarriedAndRevisionIntroducedBlockers(t *testing.T) {
	l := newReviewLoop(t)
	l.verdict(t, "in_progress", categorized("P1", "missing_rollback", "no rollback step")).Want(http.StatusOK)
	l.backToReview(t)
	l.verdict(t, "in_progress",
		categorized("P1", "missing_rollback", "No rollback step "),
		categorized("P1", "scope_creep", "the fix added an unrelated endpoint", map[string]any{"introduced_by_revision": true}),
		categorized("P2", "missing_test_first", "a new P2 is not a blocker, so it is free"),
	).Want(http.StatusOK)
}

func TestReviewFindings_RoundOneListsEveryBlockerWithoutFlags(t *testing.T) {
	l := newReviewLoop(t)
	l.verdict(t, "in_progress",
		categorized("P0", "wrong_owner", "a"), categorized("P1", "scope_creep", "b"),
	).Want(http.StatusOK)
}

func TestReviewFindings_TopCategoriesLast14Days(t *testing.T) {
	l := newReviewLoop(t)
	seed := func(findings, age string) {
		dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details, created_at)
			SELECT workspace_id, id, 'agent', $2, 'review_changes_requested', jsonb_build_object('findings', $3::text), now() - $4::interval FROM issue WHERE id = $1`,
			l.issueID, l.agentID, findings, age)
	}
	seed(`[{"severity":"P1","summary":"x","category":"scope_creep"},{"severity":"P2","summary":"y","category":"scope_creep"}]`, "1 day")
	seed(`[{"severity":"P1","summary":"x","category":"missing_rollback"}]`, "2 days")
	seed(`[{"severity":"P1","summary":"x"}]`, "3 days")
	seed(`[{"severity":"P1","summary":"x","category":"old_category"}]`, "20 days")

	var resp struct {
		Days       int `json:"days"`
		Categories []struct {
			Category string `json:"category"`
			Count    int    `json:"count"`
		} `json:"top_categories"`
		Findings []struct {
			Category string `json:"category"`
		} `json:"findings"`
	}
	testutil.Call(t, testHandler.ListReviewFindingCategories, newRequest("GET", "/api/issues/review-findings?days=14&limit=2", nil)).
		Want(http.StatusOK).JSON(&resp)
	if resp.Days != 14 || len(resp.Categories) != 2 || resp.Categories[0].Category != "scope_creep" || resp.Categories[0].Count != 2 {
		t.Fatalf("top categories = %+v", resp)
	}
	for _, c := range resp.Categories {
		if c.Category == "old_category" {
			t.Fatalf("a 20-day-old category is in the 14-day top: %+v", resp.Categories)
		}
	}
	if len(resp.Findings) != 4 {
		t.Fatalf("findings in window = %d, want 4", len(resp.Findings))
	}
}
