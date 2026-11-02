package handler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// explainSearchPlan returns the EXPLAIN text of the search statement against
// the given workspace, with the planner left on its defaults.
func explainSearchPlan(t *testing.T, wsID, phrase string) string {
	t.Helper()
	ctx := context.Background()
	query, args := buildSearchQuery(phrase, splitSearchTerms(phrase), 0, false, false, []string{"done", "cancelled"})
	args[3] = util.MustParseUUID(wsID)
	args[len(args)-2], args[len(args)-1] = 20, 0

	rows, err := testPool.Query(ctx, "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

// TestIssueSearch_PlanReachesTrigramIndexes pins the CHE-3164 fix: the folded
// side tables carry trigram GIN indexes and the search statement's text
// predicates sit where the planner uses them. Before, the candidate scan read
// every title, description and comment of the workspace. The workspace is the
// benchmark corpus at a smaller size; the planner chooses on its own.
func TestIssueSearch_PlanReachesTrigramIndexes(t *testing.T) {
	ctx := context.Background()
	const slug = "che3164-plan-test"
	wsID := seedSearchBench(t, ctx, slug, 3000)
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM comment WHERE workspace_id = $1`,
			`DELETE FROM issue WHERE workspace_id = $1`,
			`DELETE FROM workspace WHERE id = $1`,
		} {
			if _, err := testPool.Exec(ctx, stmt, wsID); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})

	// Earlier tests leave dead rows in the side tables; the heap-vs-index cost
	// gap is small enough for them to flip the choice, so plan on a clean heap.
	if _, err := testPool.Exec(ctx, `VACUUM (ANALYZE) issue_search_text, comment_search_text`); err != nil {
		t.Fatal(err)
	}

	for _, phrase := range []string{"login bug", "Đăng nhập"} {
		t.Run(phrase, func(t *testing.T) {
			plan := explainSearchPlan(t, wsID, phrase)
			for _, index := range []string{
				"idx_issue_search_text_title_trgm",
				"idx_issue_search_text_description_trgm",
				"idx_comment_search_text_content_trgm",
			} {
				if !strings.Contains(plan, index) {
					t.Errorf("plan does not use %s:\n%s", index, plan)
				}
			}
		})
	}
}

// TestIssueSearch_TrigramPrefilterKeepsEveryEligibleIssue guards the result set
// the prefilter must not change: an issue that matches only through its number,
// only through one comment of several, with terms spread over title, description
// and comments, or with a term too short for a trigram, still comes back, in the
// order the unfiltered legacy statement gives.
func TestIssueSearch_TrigramPrefilterKeepsEveryEligibleIssue(t *testing.T) {
	token := fmt.Sprintf("trg%d", time.Now().UnixNano())
	baseTime := time.Now().Add(-time.Hour).UTC()

	spread := dbfx.Issue(t, token+" alpha", testutil.Cols{"description": "plain", "updated_at": baseTime.Add(time.Minute)})
	dbfx.Comment(t, spread, "beta only here", testutil.Cols{"created_at": baseTime.Add(time.Minute)})
	dbfx.Issue(t, "unrelated "+token+"zz", testutil.Cols{"description": "ab cd", "updated_at": baseTime.Add(2 * time.Minute)})
	dbfx.Issue(t, "ab marker "+token, testutil.Cols{"description": "x", "updated_at": baseTime.Add(3 * time.Minute)})
	commentOnly := dbfx.Issue(t, "no title hit", testutil.Cols{"updated_at": baseTime.Add(4 * time.Minute)})
	dbfx.Comment(t, commentOnly, "older "+token+" mention", testutil.Cols{"created_at": baseTime.Add(4 * time.Minute)})
	dbfx.Comment(t, commentOnly, "newer unrelated", testutil.Cols{"created_at": baseTime.Add(5 * time.Minute)})

	for _, phrase := range []string{token, token + " beta", "ab " + token} {
		t.Run(phrase, func(t *testing.T) {
			terms := splitSearchTerms(phrase)
			queryNum, hasNum := parseQueryNumber(phrase)
			keys := []string{"done", "cancelled"}
			candidate := runBuiltSearchForParity(t, phrase, false, keys, 50, 0)
			legacyQuery, legacyArgs := buildLegacySearchQueryForParity(phrase, terms, queryNum, hasNum, false, keys)
			legacyArgs[3] = testWorkspaceID
			legacyArgs[len(legacyArgs)-2], legacyArgs[len(legacyArgs)-1] = 50, 0
			legacy := runLegacySearchForParity(t, legacyQuery, legacyArgs)
			if fmt.Sprint(candidate) != fmt.Sprint(legacy) {
				t.Fatalf("prefiltered rows differ from legacy\ncandidate: %v\nlegacy:    %v", candidate, legacy)
			}
		})
	}
}
