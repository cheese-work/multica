package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7677: the author handler resolves to the issue's sticky author
// (issue_step_assignee, reserved key), else capability_routing[type].author,
// else it hands off to nobody.

type authorCase struct {
	projectID           string
	authorID, reviewer  string
	otherAuthor, person string
}

func newAuthorCase(t *testing.T) authorCase {
	t.Helper()
	seedTestCatalog(t)
	first := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, first).Scan(&runtimeID)
	c := authorCase{
		authorID:    first,
		reviewer:    dbfx.Agent(t, "Author reviewer "+workflowTestSuffix(), runtimeID),
		otherAuthor: dbfx.Agent(t, "Author other "+workflowTestSuffix(), runtimeID),
		person:      dbfx.Agent(t, "Author reassigned "+workflowTestSuffix(), runtimeID),
	}
	c.projectID = createWorkflowTestProject(t, "Author handler project "+workflowTestSuffix())
	setColumnHandoff(t, c.projectID, "in_progress", map[string]any{
		"handler": map[string]any{"type": "author"}, "instructions": "Implement it.",
	}).Want(http.StatusOK)
	return c
}

// route stores capability_routing for the test workspace and removes it after.
func (c authorCase) route(t *testing.T, author string) {
	t.Helper()
	table, _ := json.Marshal(map[string]any{"feat": map[string]any{"author": author, "reviewers": []string{c.reviewer}}})
	dbfx.Exec(t, `UPDATE workspace SET settings = settings || jsonb_build_object('capability_routing', $2::jsonb) WHERE id = $1`, testWorkspaceID, string(table))
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'capability_routing' WHERE id = $1`, testWorkspaceID)
	})
}

func (c authorCase) issue(t *testing.T) string {
	t.Helper()
	return dbfx.Issue(t, "feat(x): author handler", testutil.Cols{"project_id": c.projectID, "status": "todo"})
}

func authorOf(t *testing.T, issueID string) (typ, id string, found bool) {
	t.Helper()
	return stickyAssignee(t, issueID, issueworkflow.AuthorStepKey)
}

func TestAuthorHandler_FirstEntryResolvesFromTheTable(t *testing.T) {
	c := newAuthorCase(t)
	c.route(t, c.authorID)
	issueID := c.issue(t)

	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	if status, typ, id := issueStatusAndAssignee(t, issueID); status != "in_progress" || typ != "agent" || id != c.authorID {
		t.Fatalf("issue = (%s, %s, %s), want in_progress assigned to the table author %s", status, typ, id, c.authorID)
	}
	if typ, id, found := authorOf(t, issueID); !found || typ != "agent" || id != c.authorID {
		t.Fatalf("sticky author = (%s, %s, found=%v), want agent %s", typ, id, found, c.authorID)
	}
}

func TestAuthorHandler_TableChangeKeepsTheAuthor(t *testing.T) {
	c := newAuthorCase(t)
	c.route(t, c.authorID)
	issueID := c.issue(t)
	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	c.route(t, c.otherAuthor)
	updateIssueForTest(t, issueID, map[string]any{"status": "todo"}).Want(http.StatusOK)
	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	if _, _, id := issueStatusAndAssignee(t, issueID); id != c.authorID {
		t.Fatalf("assignee after a table change = %s, want the first author %s", id, c.authorID)
	}
}

func TestAuthorHandler_PersonReassignmentUpdatesTheAuthor(t *testing.T) {
	c := newAuthorCase(t)
	c.route(t, c.authorID)
	issueID := c.issue(t)
	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	updateIssueForTest(t, issueID, map[string]any{"assignee_type": "agent", "assignee_id": c.person}).Want(http.StatusOK)
	if typ, id, found := authorOf(t, issueID); !found || typ != "agent" || id != c.person {
		t.Fatalf("sticky author after reassignment = (%s, %s, found=%v), want agent %s", typ, id, found, c.person)
	}

	updateIssueForTest(t, issueID, map[string]any{"status": "todo"}).Want(http.StatusOK)
	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	if _, _, id := issueStatusAndAssignee(t, issueID); id != c.person {
		t.Fatalf("assignee on re-entry = %s, want the reassigned author %s", id, c.person)
	}
}

func TestAuthorHandler_NoTableAndNoAuthorHandsOffToNobody(t *testing.T) {
	c := newAuthorCase(t)
	issueID := c.issue(t)

	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	if _, typ, id := issueStatusAndAssignee(t, issueID); typ != "" || id != "" {
		t.Fatalf("assignee = (%s, %s), want nobody", typ, id)
	}
	if _, _, found := authorOf(t, issueID); found {
		t.Fatal("a move that resolved no author left a sticky row")
	}
}
