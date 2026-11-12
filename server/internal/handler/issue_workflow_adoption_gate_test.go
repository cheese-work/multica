package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// adoption_linked close gate (CHE-3487, CHE-2948 S3). The gate rides the
// entry-gate engine: these tests drive a real status write into done.

const adoptionReason = "checked the agent-facing prompts, skills and runtime config; none depend on the bumped component"

func adoptionSteps(mode string) []map[string]any {
	return []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "todo"},
		{"status_key": "in_progress"},
		{"status_key": "done", "entry": []map[string]any{gate("adoption_linked", mode)}},
		{"status_key": "blocked"},
	}
}

type adoptionCase struct {
	edgeCase
	propertyID string
	projectID  string
}

// newAdoptionCase is an in_progress issue in a guarded project, with the
// Setup change property defined and a two-component registry.
func newAdoptionCase(t *testing.T, installationID int64, mode string) adoptionCase {
	t.Helper()
	c := newEdgeCase(t, installationID, adoptionSteps(mode))
	prior := testHandler.cfg.SetupComponents
	testHandler.cfg.SetupComponents = []string{"hermes", "multica"}
	t.Cleanup(func() { testHandler.cfg.SetupComponents = prior })
	dbfx.Exec(t, `DELETE FROM issue_property WHERE workspace_id = $1 AND name = 'Setup change'`, testWorkspaceID)
	propertyID := dbfx.Insert(t, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Setup change", "type": "select",
		"config": `{"options":[{"id":"opt-none","name":"none"},{"id":"opt-yes","name":"yes"}]}`})
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM issue_property WHERE id = $1`, propertyID) })
	var projectID string
	dbfx.QueryRow(t, `SELECT project_id FROM issue WHERE id = $1`, c.issueID).Scan(&projectID)
	c.enter(t, "in_progress")
	return adoptionCase{edgeCase: c, propertyID: propertyID, projectID: projectID}
}

// set writes the Setup change value ("" leaves it unset) and the metadata.
func (a adoptionCase) set(t *testing.T, value string, metadata map[string]any) {
	t.Helper()
	properties := "{}"
	if value != "" {
		properties = fmt.Sprintf(`{%q:%q}`, a.propertyID, "opt-"+value)
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	meta, _ := json.Marshal(metadata)
	dbfx.Exec(t, `UPDATE issue SET properties = $2::jsonb, metadata = $3::jsonb WHERE id = $1`, a.issueID, properties, string(meta))
}

func (a adoptionCase) close(t *testing.T, actor ...func(*http.Request) *http.Request) *testutil.Response {
	t.Helper()
	return putIssue(t, a.issueID, map[string]any{"status": "done"}, actor...)
}

func (a adoptionCase) wantRefused(t *testing.T, res *testutil.Response, factPart string) {
	t.Helper()
	res.Want(http.StatusConflict)
	if code, _ := res.Map()["code"].(string); code != "entry_gate_failed" {
		t.Fatalf("code = %q, want entry_gate_failed; body %s", code, res.Text())
	}
	if !strings.Contains(res.Text(), factPart) {
		t.Fatalf("refusal should mention %q: %s", factPart, res.Text())
	}
	wantIssueStatus(t, a.issueID, "in_progress")
}

// linkedIssue is a workspace issue in the status, as an identifier.
func linkedIssue(t *testing.T, status string) string {
	t.Helper()
	id := createTestIssue(t, "adoption "+workflowTestSuffix(), "todo", "medium")
	t.Cleanup(func() { deleteTestIssue(t, id) })
	dbfx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, id, status)
	var number int
	dbfx.QueryRow(t, `SELECT number FROM issue WHERE id = $1`, id).Scan(&number)
	return fmt.Sprintf("%s-%d", testHandler.getIssuePrefix(context.Background(), parseUUID(testWorkspaceID)), number)
}

func TestAdoptionGateFailsClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, a adoptionCase)
		fact  string
	}{
		{"property unset", func(t *testing.T, a adoptionCase) { a.set(t, "", nil) }, "Setup change is not set"},
		{"metadata unparsable", func(t *testing.T, a adoptionCase) {
			a.set(t, "yes", map[string]any{"components": "hermes", "adoption_issues": 5})
		}, "adoption_issues"},
		{"listed issue missing", func(t *testing.T, a adoptionCase) {
			a.set(t, "yes", map[string]any{"components": "hermes", "adoption_issues": "CHE-99999999"})
		}, "CHE-99999999"},
		{"listed issue of another prefix", func(t *testing.T, a adoptionCase) {
			a.set(t, "yes", map[string]any{"components": "hermes", "adoption_issues": "ZZZ-1"})
		}, "ZZZ-1"},
		{"lookup error", func(t *testing.T, a adoptionCase) {
			a.set(t, "none", nil)
			dbfx.Exec(t, `UPDATE issue_property SET config = '{"options":"broken"}' WHERE id = $1`, a.propertyID)
		}, "Setup change"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdoptionCase(t, int64(3487100+i), "block")
			tc.setup(t, a)
			a.wantRefused(t, a.close(t), tc.fact)
		})
	}
}

func TestAdoptionGateOpenLinkRefusesForBothValues(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for i, tc := range []struct {
		value, adoptionNone string
	}{{"yes", ""}, {"yes", adoptionReason}, {"none", ""}, {"none", adoptionReason}} {
		t.Run(fmt.Sprintf("%s with adoption_none %t", tc.value, tc.adoptionNone != ""), func(t *testing.T) {
			a := newAdoptionCase(t, int64(3487200+i), "block")
			open := linkedIssue(t, "in_progress")
			meta := map[string]any{"components": "hermes", "adoption_issues": open}
			if tc.adoptionNone != "" {
				meta["adoption_none"] = tc.adoptionNone
			}
			a.set(t, tc.value, meta)
			a.wantRefused(t, a.close(t), open)
		})
	}
}

func TestAdoptionGateNoneWithPassingTitleAndOpenLinkRefuses(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	a := newAdoptionCase(t, 3487250, "block")
	dbfx.Exec(t, `UPDATE issue SET title = 'Tidy the docs index' WHERE id = $1`, a.issueID)
	open := linkedIssue(t, "todo")
	a.set(t, "none", map[string]any{"adoption_issues": open})
	a.wantRefused(t, a.close(t), open)
}

func TestAdoptionGateAllows(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, a adoptionCase)
	}{
		{"yes, all links terminal", func(t *testing.T, a adoptionCase) {
			a.set(t, "yes", map[string]any{"components": "hermes", "adoption_issues": linkedIssue(t, "done") + ", " + linkedIssue(t, "cancelled")})
		}},
		{"yes, no links, adoption_none of 40+ characters", func(t *testing.T, a adoptionCase) {
			a.set(t, "yes", map[string]any{"components": "multica", "adoption_none": adoptionReason})
		}},
		{"none, cross-check passes", func(t *testing.T, a adoptionCase) {
			dbfx.Exec(t, `UPDATE issue SET title = 'Tidy the docs index' WHERE id = $1`, a.issueID)
			a.set(t, "none", nil)
		}},
		{"none, listed issues terminal", func(t *testing.T, a adoptionCase) {
			dbfx.Exec(t, `UPDATE issue SET title = 'Tidy the docs index' WHERE id = $1`, a.issueID)
			a.set(t, "none", map[string]any{"adoption_issues": linkedIssue(t, "done")})
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdoptionCase(t, int64(3487300+i), "block")
			tc.setup(t, a)
			a.close(t).Want(http.StatusOK)
			wantIssueStatus(t, a.issueID, "done")
		})
	}
}

func TestAdoptionGateYesNeedsRegistryComponent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for i, components := range []any{nil, "nope"} {
		a := newAdoptionCase(t, int64(3487350+i), "block")
		meta := map[string]any{"adoption_none": adoptionReason}
		if components != nil {
			meta["components"] = components
		}
		a.set(t, "yes", meta)
		a.wantRefused(t, a.close(t), "components")
	}
}

func TestAdoptionGateNoneCrossCheck(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Run("title names a registry component with a change word", func(t *testing.T) {
		a := newAdoptionCase(t, 3487400, "block")
		dbfx.Exec(t, `UPDATE issue SET title = 'Upgrade hermes to 2.0' WHERE id = $1`, a.issueID)
		a.set(t, "none", nil)
		a.wantRefused(t, a.close(t), "hermes")
	})
	t.Run("description names it", func(t *testing.T) {
		a := newAdoptionCase(t, 3487401, "block")
		dbfx.Exec(t, `UPDATE issue SET title = 'Weekly chore', description = 'bump multica on c00' WHERE id = $1`, a.issueID)
		a.set(t, "none", nil)
		a.wantRefused(t, a.close(t), "multica")
	})
	t.Run("a component without a change word passes", func(t *testing.T) {
		a := newAdoptionCase(t, 3487402, "block")
		dbfx.Exec(t, `UPDATE issue SET title = 'Document hermes routing' WHERE id = $1`, a.issueID)
		a.set(t, "none", nil)
		a.close(t).Want(http.StatusOK)
	})
	t.Run("a person overrides with a recorded reason", func(t *testing.T) {
		a := newAdoptionCase(t, 3487403, "block")
		dbfx.Exec(t, `UPDATE issue SET title = 'Upgrade hermes to 2.0' WHERE id = $1`, a.issueID)
		a.set(t, "none", nil)
		putIssue(t, a.issueID, map[string]any{"status": "done", "entry_gate_override_reason": "dry run only, nothing installed"}).Want(http.StatusOK)
		if n := activityCount(t, a.issueID, "entry_gate_override"); n != 1 {
			t.Fatalf("entry_gate_override rows = %d, want 1", n)
		}
	})
	t.Run("a task token never overrides", func(t *testing.T) {
		a := newAdoptionCase(t, 3487404, "block")
		dbfx.Exec(t, `UPDATE issue SET title = 'Upgrade hermes to 2.0' WHERE id = $1`, a.issueID)
		a.set(t, "none", nil)
		res := putIssue(t, a.issueID, map[string]any{"status": "done", "entry_gate_override_reason": "dry run only, nothing installed"}, func(r *http.Request) *http.Request { return asAgentTask(t, r) })
		a.wantRefused(t, res, "hermes")
	})
}

// A close that waits on the issue lock reads the link a concurrent writer
// appended: the gate cannot pass on facts older than the lock.
func TestAdoptionGateSerializesWithLinkAppend(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	a := newAdoptionCase(t, 3487500, "block")
	dbfx.Exec(t, `UPDATE issue SET title = 'Tidy the docs index' WHERE id = $1`, a.issueID)
	a.set(t, "none", nil)
	open := linkedIssue(t, "in_progress")

	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM issue WHERE id = $1 FOR UPDATE`, a.issueID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var res *testutil.Response
	finished := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		res = a.close(t)
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("the close ran while another writer held the issue lock")
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `UPDATE issue SET metadata = metadata || jsonb_build_object('adoption_issues', $2::text) WHERE id = $1`, a.issueID, open); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	a.wantRefused(t, res, open)
}

func TestAdoptionGateCreateNeedsSetupChange(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	a := newAdoptionCase(t, 3487600, "block")
	create := func(body map[string]any) *testutil.Response {
		body["project_id"] = a.projectID
		return testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body))
	}
	res := create(map[string]any{"title": "no setup change " + workflowTestSuffix()})
	res.Want(http.StatusConflict)
	if code, _ := res.Map()["code"].(string); code != "entry_gate_failed" || !strings.Contains(res.Text(), "Setup change") {
		t.Fatalf("create without Setup change: %s", res.Text())
	}
	create(map[string]any{"title": "with setup change " + workflowTestSuffix(), "properties": map[string]any{a.propertyID: "opt-none"}}).Want(http.StatusCreated)
}

func TestAdoptionGateWarnModeDoesNotRefuse(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	a := newAdoptionCase(t, 3487650, "warn")
	a.set(t, "", nil)
	a.close(t).Want(http.StatusOK)
	if n := activityCount(t, a.issueID, "entry_gate_warning"); n != 1 {
		t.Fatalf("entry_gate_warning rows = %d, want 1", n)
	}
	res := testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID,
		map[string]any{"title": "warn create " + workflowTestSuffix(), "project_id": a.projectID}))
	res.Want(http.StatusCreated)
}
