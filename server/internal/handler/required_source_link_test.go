package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-408: agent-authored issue descriptions and comments must cite the source
// the issue declares. The disposable-path case these tests exist for is an
// agent writing "Read the authoritative 01-22 plan" — prose that names a source
// the reader cannot follow — and the platform accepting it.
//
// Every rejection case asserts the DB row too, not just the status code. A 422
// that still wrote the content would satisfy a status-only assertion while
// failing the actual requirement.

const che408Plan = "https://github.com/cheese-work/pocket-actual/blob/bc40d61ee615903f5511b69ddfa7f57c2020171e/.planning/phases/01-trusted-present-day-recovery/01-22-PLAN.md"

const (
	che408Disposable = "Read the authoritative 01-22 plan"
	che408Linked     = "Read the [authoritative 01-22 plan](" + che408Plan + ")"
)

// che408EnableWorkspacePolicy opts the test workspace in and restores the
// previous settings blob afterwards, so this file cannot leak policy state into
// tests that assume enforcement is off.
func che408EnableWorkspacePolicy(t *testing.T) {
	t.Helper()
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`,
		[]byte(`{"require_source_link": true}`), testWorkspaceID)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, previous, testWorkspaceID)
	})
}

// che408DeclareSource creates the url-typed "Required source" property and sets
// it on issueID, returning the property id.
//
// The declaration deliberately travels through a property rather than the issue
// body: the body is the untrusted text under test, and an in-description marker
// would let its author delete the obligation it is being held to.
func che408DeclareSource(t *testing.T, issueID, source string) string {
	t.Helper()
	propertyID := dbfx.Insert(t, "issue_property", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"name":         "Required source",
		"type":         "url",
		"position":     1,
	})
	value, err := json.Marshal(map[string]string{propertyID: source})
	if err != nil {
		t.Fatalf("marshal property value: %v", err)
	}
	dbfx.Exec(t, `UPDATE issue SET properties = $1 WHERE id = $2`, value, issueID)
	return propertyID
}

// issueDescription reads a description that may still be NULL — dbfx.Issue
// creates issues without one, and "no description" is exactly the before-state
// these rejection assertions need to compare against.
func issueDescription(t *testing.T, issueID string) string {
	t.Helper()
	var description *string
	dbfx.QueryRow(t, `SELECT description FROM issue WHERE id = $1`, issueID).Scan(&description)
	if description == nil {
		return ""
	}
	return *description
}

func che408AgentActor(t *testing.T, email, agentName string) (userID, agentID, taskID string) {
	t.Helper()
	return governedInstructionActorFixture(t, email, agentName)
}

// ── UpdateIssue (site 1) ─────────────────────────────────────────────────

func TestUpdateIssue_AgentDescriptionMustCiteRequiredSource(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-update-issue@multica.test", "che408-update-issue")

	issueID := dbfx.Issue(t, "che408 update target")
	che408DeclareSource(t, issueID, che408Plan)

	before := issueDescription(t, issueID)

	req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/issues/"+issueID, map[string]any{
		"description": che408Disposable,
	}), "id", issueID)
	req = asAgentActor(req, agentID, taskID)

	w := testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusUnprocessableEntity)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["code"] != "required_source_link_missing" {
		t.Errorf("code = %v, want required_source_link_missing", body["code"])
	}
	if body["required_source"] != che408Plan {
		t.Errorf("required_source = %v, want the declared plan URL", body["required_source"])
	}

	if after := issueDescription(t, issueID); after != before {
		t.Errorf("description must be unchanged on rejection, got %q", after)
	}
}

func TestUpdateIssue_AgentDescriptionWithLinkIsAccepted(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-update-issue-ok@multica.test", "che408-update-issue-ok")

	issueID := dbfx.Issue(t, "che408 update accept")
	che408DeclareSource(t, issueID, che408Plan)

	req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/issues/"+issueID, map[string]any{
		"description": che408Linked,
	}), "id", issueID)
	req = asAgentActor(req, agentID, taskID)

	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)

	if after := issueDescription(t, issueID); after != che408Linked {
		t.Errorf("description = %q, want the linked body written unchanged", after)
	}
}

// A cloud-node PAT is the same threat class as an mat_ token, and resolveActor
// deliberately does not classify it as "agent" — so a guard keyed only on
// actorType would leave this path open (CHE-455's isMachineCredentialActor).
func TestUpdateIssue_CloudNodeActorIsAlsoBound(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID := createPlainMember(t, "che408-cloud-node@multica.test")

	issueID := dbfx.Issue(t, "che408 cloud node")
	che408DeclareSource(t, issueID, che408Plan)

	req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/issues/"+issueID, map[string]any{
		"description": che408Disposable,
	}), "id", issueID)
	req = asCloudNodeActor(req)

	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusUnprocessableEntity)
}

// Humans are out of scope by design: a person writing prose about a plan is
// making an editorial choice, not evading a contract.
func TestUpdateIssue_HumanActorIsNotBound(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID := createPlainMember(t, "che408-human@multica.test")

	issueID := dbfx.Issue(t, "che408 human")
	che408DeclareSource(t, issueID, che408Plan)

	req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/issues/"+issueID, map[string]any{
		"description": che408Disposable,
	}), "id", issueID)

	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
}

// Opt-in (Cheese, CHE-408): a workspace that never enabled the policy keeps its
// existing behaviour, declared source or not.
func TestUpdateIssue_PolicyOffLeavesAgentWritesAlone(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	userID, agentID, taskID := che408AgentActor(t, "che408-policy-off@multica.test", "che408-policy-off")

	issueID := dbfx.Issue(t, "che408 policy off")
	che408DeclareSource(t, issueID, che408Plan)

	req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/issues/"+issueID, map[string]any{
		"description": che408Disposable,
	}), "id", issueID)
	req = asAgentActor(req, agentID, taskID)

	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
}

// No declaration, no obligation — the ordinary case for most issues.
func TestUpdateIssue_NoDeclaredSourceAllowsAnyBody(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-no-source@multica.test", "che408-no-source")

	issueID := dbfx.Issue(t, "che408 no declared source")

	req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/issues/"+issueID, map[string]any{
		"description": che408Disposable,
	}), "id", issueID)
	req = asAgentActor(req, agentID, taskID)

	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
}

// The ways a destination can be present in the text without being a citation.
func TestUpdateIssue_NonCitingBodiesAreRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-shapes@multica.test", "che408-shapes")

	cases := []struct {
		name string
		body string
	}{
		{name: "prose only", body: che408Disposable},
		{name: "code span", body: "the plan is `[x](" + che408Plan + ")`"},
		{name: "fenced block", body: "```\n[x](" + che408Plan + ")\n```"},
		{name: "quoted only", body: "> they linked [x](" + che408Plan + ")"},
		{name: "different url", body: "see [x](https://github.com/cheese-work/pocket-actual/blob/main/README.md)"},
		{name: "different revision", body: "see [x](https://github.com/cheese-work/pocket-actual/blob/main/.planning/phases/01-trusted-present-day-recovery/01-22-PLAN.md)"},
		{name: "placeholder", body: "see [x](TODO)"},
		{name: "runtime local", body: "see [x](file:///home/agent/workdir/01-22-PLAN.md)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issueID := dbfx.Issue(t, "che408 shape "+tc.name)
			che408DeclareSource(t, issueID, che408Plan)

			before := issueDescription(t, issueID)

			req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/issues/"+issueID, map[string]any{
				"description": tc.body,
			}), "id", issueID)
			req = asAgentActor(req, agentID, taskID)

			testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusUnprocessableEntity)

			if after := issueDescription(t, issueID); after != before {
				t.Errorf("description must be unchanged, got %q", after)
			}
		})
	}
}

// encoding/json matches object keys case-insensitively, so a case-variant key
// still populates the struct field the write path branches on. This is the
// exact bypass found on PR #29 for CHE-455.
func TestUpdateIssue_CaseVariantDescriptionKeyIsStillBound(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-case@multica.test", "che408-case")

	for _, key := range []string{"Description", "DESCRIPTION"} {
		t.Run(key, func(t *testing.T) {
			issueID := dbfx.Issue(t, "che408 case "+key)
			che408DeclareSource(t, issueID, che408Plan)

			before := issueDescription(t, issueID)

			req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/issues/"+issueID, map[string]any{
				key: che408Disposable,
			}), "id", issueID)
			req = asAgentActor(req, agentID, taskID)

			testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusUnprocessableEntity)

			if after := issueDescription(t, issueID); after != before {
				t.Errorf("description must be unchanged for key %q, got %q", key, after)
			}
		})
	}
}

// ── CreateComment (site 2) ───────────────────────────────────────────────

func TestCreateComment_AgentContentMustCiteRequiredSource(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-create-comment@multica.test", "che408-create-comment")

	issueID := dbfx.Issue(t, "che408 comment target")
	che408DeclareSource(t, issueID, che408Plan)

	req := withURLParam(newRequestAs(userID, http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content": che408Disposable,
	}), "id", issueID)
	req = asAgentActor(req, agentID, taskID)

	testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusUnprocessableEntity)

	// No comment row, and therefore nothing for triggerTasksForComment to have
	// dispatched: rejection must leave zero visible change AND zero work.
	var count int
	dbfx.QueryRow(t, `SELECT COUNT(*) FROM comment WHERE issue_id = $1`, issueID).Scan(&count)
	if count != 0 {
		t.Errorf("comment count = %d, want 0 on rejection", count)
	}
}

func TestCreateComment_AgentContentWithLinkIsAccepted(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-create-comment-ok@multica.test", "che408-create-comment-ok")

	issueID := dbfx.Issue(t, "che408 comment accept")
	che408DeclareSource(t, issueID, che408Plan)

	req := withURLParam(newRequestAs(userID, http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
		"content": che408Linked,
	}), "id", issueID)
	req = asAgentActor(req, agentID, taskID)

	testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated)

	var count int
	dbfx.QueryRow(t, `SELECT COUNT(*) FROM comment WHERE issue_id = $1`, issueID).Scan(&count)
	if count != 1 {
		t.Errorf("comment count = %d, want 1", count)
	}
}

// ── UpdateComment (site 3) ───────────────────────────────────────────────

// An edit can strip the citation out of a comment that had one, so the update
// path needs the same check as create.
func TestUpdateComment_AgentEditCannotRemoveCitation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-update-comment@multica.test", "che408-update-comment")

	issueID := dbfx.Issue(t, "che408 comment edit")
	che408DeclareSource(t, issueID, che408Plan)
	commentID := dbfx.Comment(t, issueID, che408Linked, testutil.Cols{
		"author_type": "agent",
		"author_id":   agentID,
	})

	req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/comments/"+commentID, map[string]any{
		"content": che408Disposable,
	}), "commentId", commentID)
	req = asAgentActor(req, agentID, taskID)

	testutil.Call(t, testHandler.UpdateComment, req).Want(http.StatusUnprocessableEntity)

	var after string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE id = $1`, commentID).Scan(&after)
	if after != che408Linked {
		t.Errorf("content must be unchanged on rejection, got %q", after)
	}
}

// ── CreateIssue (site 4) ─────────────────────────────────────────────────

// A new issue has no properties of its own, so a create carries an obligation
// only by inheriting its parent's declaration.
func TestCreateIssue_AgentDescriptionMustCiteParentRequiredSource(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-create-issue@multica.test", "che408-create-issue")

	parentID := dbfx.Issue(t, "che408 parent with source")
	che408DeclareSource(t, parentID, che408Plan)

	var before int
	dbfx.QueryRow(t, `SELECT COUNT(*) FROM issue WHERE parent_issue_id = $1`, parentID).Scan(&before)

	req := newRequestAs(userID, http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":           "che408 child",
		"description":     che408Disposable,
		"parent_issue_id": parentID,
		"workspace_id":    testWorkspaceID,
	})
	req = asAgentActor(req, agentID, taskID)

	testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusUnprocessableEntity)

	var after int
	dbfx.QueryRow(t, `SELECT COUNT(*) FROM issue WHERE parent_issue_id = $1`, parentID).Scan(&after)
	if after != before {
		t.Errorf("child issue count = %d, want %d — rejection must create nothing", after, before)
	}
}

func TestCreateIssue_AgentDescriptionWithLinkIsAccepted(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-create-issue-ok@multica.test", "che408-create-issue-ok")

	parentID := dbfx.Issue(t, "che408 parent accept")
	che408DeclareSource(t, parentID, che408Plan)

	req := newRequestAs(userID, http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":           "che408 child ok",
		"description":     che408Linked,
		"parent_issue_id": parentID,
		"workspace_id":    testWorkspaceID,
	})
	req = asAgentActor(req, agentID, taskID)

	testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated)
}

// ── Policy reader ────────────────────────────────────────────────────────

// Opt-in when absent, but fail CLOSED when the blob cannot be read: unparseable
// settings mean we cannot show the workspace opted out, and for a security
// control the safe reading of "cannot tell" is to enforce.
func TestRequiredSourceLinkEnabledDefaults(t *testing.T) {
	cases := []struct {
		name     string
		settings string
		want     bool
	}{
		{name: "absent blob", settings: "", want: false},
		{name: "empty object", settings: `{}`, want: false},
		{name: "explicit false", settings: `{"require_source_link": false}`, want: false},
		{name: "explicit true", settings: `{"require_source_link": true}`, want: true},
		{name: "other keys only", settings: `{"github_enabled": true}`, want: false},
		{name: "malformed fails closed", settings: `{"require_source_link":`, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := db.Workspace{Settings: []byte(tc.settings)}
			if got := requiredSourceLinkEnabled(ws); got != tc.want {
				t.Errorf("requiredSourceLinkEnabled(%q) = %v, want %v", tc.settings, got, tc.want)
			}
		})
	}
}
