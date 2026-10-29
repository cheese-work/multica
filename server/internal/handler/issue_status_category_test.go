package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Category changes on custom statuses (CHE-2274). An owner/admin may move a
// custom status to another lifecycle category, but only with explicit intent:
// the change rewrites the machine semantics of every issue already on it.

type categoryChangeConflict struct {
	Code       string `json:"code"`
	IssueCount int64  `json:"issue_count"`
	From       string `json:"from_category"`
	To         string `json:"to_category"`
}

func patchIssueStatus(t *testing.T, entry db.IssueStatus, body map[string]any, userID string) *testutil.Response {
	t.Helper()
	req := newRequestAs(userID, http.MethodPatch, "/api/issue-statuses/"+uuidToString(entry.ID), body)
	return testutil.Call(t, testHandler.UpdateIssueStatus, withURLParam(req, "id", uuidToString(entry.ID)))
}

func storedStatusCategory(t *testing.T, entry db.IssueStatus) string {
	t.Helper()
	var category string
	dbfx.QueryRow(t, `SELECT category FROM issue_status WHERE id = $1`, entry.ID).Scan(&category)
	return category
}

func TestUpdateIssueStatusCategory(t *testing.T) {
	t.Run("owner moves a custom status with confirmation", func(t *testing.T) {
		entry := createTestCustomStatus(t, "cat_move_ok", "done")
		dbfx.Issue(t, "on the status 1", testutil.Cols{"status": entry.Key})
		dbfx.Issue(t, "on the status 2", testutil.Cols{"status": entry.Key})

		var out struct {
			Category   string `json:"category"`
			Key        string `json:"key"`
			IssueCount int64  `json:"issue_count"`
		}
		patchIssueStatus(t, entry, map[string]any{"category": "started", "confirm_category_change": true}, testUserID).
			Want(http.StatusOK).JSON(&out)
		// The response keeps the installed-client wire enum; clients normalize it.
		if category, _ := issuestatus.ParseCategory(out.Category); category != "started" || out.Key != entry.Key || out.IssueCount != 2 {
			t.Fatalf("response = %+v, want started/%s with issue_count 2", out, entry.Key)
		}
		if got := storedStatusCategory(t, entry); got != "started" {
			t.Fatalf("stored category = %q, want started", got)
		}
	})

	t.Run("owner without confirmation gets the issue count and nothing changes", func(t *testing.T) {
		entry := createTestCustomStatus(t, "cat_move_unconfirmed", "done")
		dbfx.Issue(t, "on the status", testutil.Cols{"status": entry.Key})

		var conflict categoryChangeConflict
		patchIssueStatus(t, entry, map[string]any{"category": "started"}, testUserID).
			Want(http.StatusConflict).JSON(&conflict)
		if conflict.Code != "category_change_requires_confirmation" || conflict.IssueCount != 1 ||
			conflict.From != "done" || conflict.To != "started" {
			t.Fatalf("conflict = %+v", conflict)
		}
		if got := storedStatusCategory(t, entry); got != "done" {
			t.Fatalf("stored category = %q, want done", got)
		}
	})

	t.Run("same category needs no confirmation", func(t *testing.T) {
		entry := createTestCustomStatus(t, "cat_same", "done")
		patchIssueStatus(t, entry, map[string]any{"category": "done", "color": "#654321"}, testUserID).Want(http.StatusOK)
	})

	t.Run("unknown category is a bad request", func(t *testing.T) {
		entry := createTestCustomStatus(t, "cat_bogus", "done")
		patchIssueStatus(t, entry, map[string]any{"category": "bogus", "confirm_category_change": true}, testUserID).
			Want(http.StatusBadRequest)
		if got := storedStatusCategory(t, entry); got != "done" {
			t.Fatalf("stored category = %q, want done", got)
		}
	})

	t.Run("plain member is refused", func(t *testing.T) {
		entry := createTestCustomStatus(t, "cat_member", "done")
		memberUser := dbfx.User(t, "cat member", "cat-member@multica.test")
		dbfx.Member(t, testWorkspaceID, memberUser, "member")
		patchIssueStatus(t, entry, map[string]any{"category": "started", "confirm_category_change": true}, memberUser).
			Want(http.StatusForbidden)
		if got := storedStatusCategory(t, entry); got != "done" {
			t.Fatalf("stored category = %q, want done", got)
		}
	})

	t.Run("built-in status keeps its category", func(t *testing.T) {
		seedTestCatalog(t)
		builtIn, err := testHandler.Queries.GetIssueStatusEntryByKey(t.Context(), db.GetIssueStatusEntryByKeyParams{
			WorkspaceID: parseUUID(testWorkspaceID), Key: "in_review",
		})
		if err != nil {
			t.Fatal(err)
		}
		patchIssueStatus(t, builtIn, map[string]any{"category": "done", "confirm_category_change": true}, testUserID).
			Want(http.StatusForbidden)
		if got := storedStatusCategory(t, builtIn); got != "started" {
			t.Fatalf("built-in category = %q, want started", got)
		}
	})

	t.Run("key stays immutable", func(t *testing.T) {
		entry := createTestCustomStatus(t, "cat_key", "done")
		patchIssueStatus(t, entry, map[string]any{"key": "cat_key_renamed", "category": "started", "confirm_category_change": true}, testUserID).
			Want(http.StatusBadRequest)
		var key, category string
		dbfx.QueryRow(t, `SELECT key, category FROM issue_status WHERE id = $1`, entry.ID).Scan(&key, &category)
		if key != "cat_key" || category != "done" {
			t.Fatalf("stored key/category = %q/%q, want cat_key/done", key, category)
		}
	})

	t.Run("moved status lands last in its new category", func(t *testing.T) {
		entry := createTestCustomStatus(t, "cat_position", "done")
		other := createTestCustomStatus(t, "cat_position_peer", "started")
		patchIssueStatus(t, entry, map[string]any{"category": "started", "confirm_category_change": true}, testUserID).
			Want(http.StatusOK)
		var moved, peer float64
		dbfx.QueryRow(t, `SELECT position FROM issue_status WHERE id = $1`, entry.ID).Scan(&moved)
		dbfx.QueryRow(t, `SELECT position FROM issue_status WHERE id = $1`, other.ID).Scan(&peer)
		if moved <= peer {
			t.Fatalf("moved position %v must come after peer position %v", moved, peer)
		}
	})
}

// The done-category guard (CHE-2148) reads the category on every call, so it
// follows a category change immediately.
func TestAgentStatusGuardFollowsCategoryChange(t *testing.T) {
	entry := createTestCustomStatus(t, "guard_follow", "done")
	agentID := dbfx.Agent(t, "category guard actor", testRuntimeID)
	agent := map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": agentID}
	issueID := dbfx.Issue(t, "category guard", testutil.Cols{"status": "in_progress"})
	linkStatusGuardPR(t, issueID, "github", "open", false)
	set := func(wantCode int) {
		t.Helper()
		issueWriters[0].write(t, issueID, map[string]any{"status": entry.Key, "suppress_run": true}, agent).Want(wantCode)
	}

	set(http.StatusConflict)
	patchIssueStatus(t, entry, map[string]any{"category": "started", "confirm_category_change": true}, testUserID).Want(http.StatusOK)
	set(http.StatusOK)
	wantIssueStatus(t, issueID, entry.Key)

	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, issueID)
	patchIssueStatus(t, entry, map[string]any{"category": "done", "confirm_category_change": true}, testUserID).Want(http.StatusOK)
	set(http.StatusConflict)
}

// Issue rows carry a server-resolved status_category, so a category move must
// tell every client to refresh issue projections; a rename must not.
func TestCategoryChangeEventFlagsIssueProjections(t *testing.T) {
	published := make(chan events.Event, 8)
	testHandler.Bus.Subscribe(protocol.EventIssueStatusChanged, func(e events.Event) {
		select {
		case published <- e:
		default:
		}
	})
	categoryChanged := func(t *testing.T, body map[string]any, entry db.IssueStatus) any {
		t.Helper()
		for len(published) > 0 {
			<-published
		}
		patchIssueStatus(t, entry, body, testUserID).Want(http.StatusOK)
		payload, ok := (<-published).Payload.(map[string]any)
		if !ok {
			t.Fatalf("event payload is not a map")
		}
		return payload["category_changed"]
	}

	moved := createTestCustomStatus(t, "cat_event_move", "done")
	if got := categoryChanged(t, map[string]any{"category": "started", "confirm_category_change": true}, moved); got != true {
		t.Fatalf("category_changed = %v on a category move, want true", got)
	}
	renamed := createTestCustomStatus(t, "cat_event_rename", "done")
	if got := categoryChanged(t, map[string]any{"name": "Renamed only"}, renamed); got != nil {
		t.Fatalf("category_changed = %v on a rename, want it absent", got)
	}
}
