package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// ── Remaining write paths ────────────────────────────────────────────────

// CreateCommentSubIssue is the fifth route that can persist an agent-authored
// description (router.go: r.With(handler.RequireHumanActor).Post("/sub-issues")).
// It carries no source-link guard because the router closes it to both machine
// credential kinds — mat_ task tokens stamp X-Actor-Source: task_token and mcn_
// cloud PATs stamp cloud_pat, and RequireHumanActor rejects both. This test
// pins that reasoning: if the middleware is ever removed or its classification
// narrowed, the path becomes reachable by an agent and needs its own guard.
func TestRequireHumanActor_ClosesSubIssueCreationToMachineActors(t *testing.T) {
	reached := false
	guarded := RequireHumanActor(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	for _, source := range []string{"task_token", "cloud_pat"} {
		t.Run(source, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodPost, "/api/comments/x/sub-issues", nil)
			req.Header.Set("X-Actor-Source", source)

			rec := httptest.NewRecorder()
			guarded.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 for %s", rec.Code, source)
			}
			if reached {
				t.Errorf("%s actor reached the handler; this path would need its own source-link guard", source)
			}
		})
	}
}

// ── Fail-closed on "cannot tell" (CHE-408 review follow-up) ──────────────
//
// The guard's whole premise is that "cannot determine" resolves to enforce, the
// same way a malformed settings blob does. Three lookups sit between "policy is
// ON" and "this write is fine": decoding issue.Properties, ListIssueProperties,
// and the two handler re-fetches of the issue. Each one originally returned
// ""/skipped on error, which means a transient DB fault silently waived the
// requirement — CHE-153 reopened by a connection blip rather than by a bug in
// the check itself.
//
// These tests pin the failure direction. A cancelled request context produces a
// real query error on the live pool, which is the closest faithful stand-in for
// the connection fault being guarded against; the malformed-properties case is
// produced by writing bytes the column accepts but json.Unmarshal rejects.

// che408WantUndetermined asserts a fail-closed refusal rather than a pass.
func che408WantUndetermined(t *testing.T, body []byte, field string) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if decoded["code"] != "required_source_link_undetermined" {
		t.Errorf("code = %v, want required_source_link_undetermined", decoded["code"])
	}
	if decoded["field"] != field {
		t.Errorf("field = %v, want %q", decoded["field"], field)
	}
}

// TestDeclaredRequiredSource_UndecodablePropertiesIsAnError covers the
// json.Unmarshal(issue.Properties) branch.
//
// This one is asserted at the function rather than through a request, because
// the issue table carries a CHECK constraint (issue_properties_is_object) that
// makes the branch unreachable from a stored row: anything the column will
// accept decodes into map[string]json.RawMessage. The branch is therefore
// defense in depth against that invariant changing, and what matters is its
// DIRECTION — "cannot decode" must surface as an error the caller fails closed
// on, never as the "" that means "nothing declared". Writing it as a live
// request test would require fabricating a row the database refuses to store.
func TestDeclaredRequiredSource_UndecodablePropertiesIsAnError(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	source, err := testHandler.declaredRequiredSource(context.Background(), db.Issue{
		WorkspaceID: parseUUID(testWorkspaceID),
		Properties:  []byte(`["not an object"]`),
	})
	if err == nil {
		t.Fatal("undecodable properties must return an error, not a silent empty requirement")
	}
	if source != "" {
		t.Errorf("source = %q, want empty alongside the error", source)
	}
}

// TestUpdateIssue_PropertyLookupErrorFailsClosed covers the
// ListIssueProperties error path: the issue carries property values, so the
// definitions must be read to find out whether one of them is the required
// source, and that read is what fails.
func TestUpdateIssue_PropertyLookupErrorFailsClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-prop-lookup@multica.test", "che408-prop-lookup")

	issueID := dbfx.Issue(t, "che408 property lookup failure")
	che408DeclareSource(t, issueID, che408Plan)

	before := issueDescription(t, issueID)

	req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/issues/"+issueID, map[string]any{
		"description": che408Disposable,
	}), "id", issueID)
	req = asAgentActor(req, agentID, taskID)

	handler := che408HandlerFailingQuery(t, "FROM issue_property")
	w := testutil.Call(t, handler.UpdateIssue, req).Want(http.StatusServiceUnavailable)
	che408WantUndetermined(t, w.Body.Bytes(), "description")

	if after := issueDescription(t, issueID); after != before {
		t.Errorf("description must be unchanged when the requirement cannot be determined, got %q", after)
	}
}

// che408FailingDBTX passes every statement through to the real pool except the
// ones whose SQL contains a marker, which fail instead.
//
// Injecting at the DBTX seam rather than cancelling the request context is
// deliberate: a cancelled context fails the handler's own earlier lookups too,
// so the request would never reach the guard and the test would pass without
// exercising the line it names. Targeting one statement keeps the rest of the
// handler on the live database, so the write really does get as far as the
// check before the fault lands.
type che408FailingDBTX struct {
	inner  db.DBTX
	marker string
	// skip lets a marker that matches several statements fail only the Nth
	// one. The guard re-fetches the issue with the same SQL the handler
	// already used to load it, so failing every match would break the earlier
	// load and the request would never reach the check under test.
	skip  int
	seen  *int
	guard *sync.Mutex
}

var errChe408InjectedQueryFailure = errors.New("che408: injected query failure")

func (f che408FailingDBTX) shouldFail(sql string) bool {
	if !strings.Contains(sql, f.marker) {
		return false
	}
	if f.seen == nil {
		return true
	}
	f.guard.Lock()
	defer f.guard.Unlock()
	*f.seen++
	return *f.seen > f.skip
}

func (f che408FailingDBTX) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	if f.shouldFail(sql) {
		return pgconn.CommandTag{}, errChe408InjectedQueryFailure
	}
	return f.inner.Exec(ctx, sql, args...)
}

func (f che408FailingDBTX) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	if f.shouldFail(sql) {
		return nil, errChe408InjectedQueryFailure
	}
	return f.inner.Query(ctx, sql, args...)
}

func (f che408FailingDBTX) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	if f.shouldFail(sql) {
		return che408FailingRow{}
	}
	return f.inner.QueryRow(ctx, sql, args...)
}

type che408FailingRow struct{}

func (che408FailingRow) Scan(...any) error { return errChe408InjectedQueryFailure }

// che408HandlerFailingQuery copies the suite handler and swaps in a Queries
// whose statements matching marker fail. The copy is per-test, so no other test
// sees the fault.
func che408HandlerFailingQuery(t *testing.T, marker string) *Handler {
	t.Helper()
	faulty := *testHandler
	faulty.Queries = db.New(che408FailingDBTX{inner: testPool, marker: marker})
	return &faulty
}

// che408HandlerFailingAfter fails matching statements only after the first
// skip of them have been allowed through.
func che408HandlerFailingAfter(t *testing.T, marker string, skip int) *Handler {
	t.Helper()
	seen := 0
	faulty := *testHandler
	faulty.Queries = db.New(che408FailingDBTX{
		inner:  testPool,
		marker: marker,
		skip:   skip,
		seen:   &seen,
		guard:  &sync.Mutex{},
	})
	return &faulty
}

// che408IssueSelect is the column list unique to the issue-row SELECTs. The
// guard's re-fetch uses it, and so does the handler's own earlier load, which
// is why the counting variant above exists.
const che408IssueSelect = "triage_state FROM issue"

// TestCreateIssue_ParentLookupErrorFailsClosed covers issue.go's re-fetch of
// the PARENT issue. That lookup is what tells the guard whether the parent
// declares a required source; on error the create used to fall straight through
// to IssueService.Create, persisting an uncited description AND enqueueing the
// assignee's agent task in the same transaction.
func TestCreateIssue_ParentLookupErrorFailsClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-parent-lookup@multica.test", "che408-parent-lookup")

	parentID := dbfx.Issue(t, "che408 parent lookup failure")
	che408DeclareSource(t, parentID, che408Plan)

	req := newRequestAs(userID, http.MethodPost, "/api/issues", map[string]any{
		"title":           "che408 uncited child",
		"description":     che408Disposable,
		"parent_issue_id": parentID,
	})
	req = asAgentActor(req, agentID, taskID)

	handler := che408HandlerFailingQuery(t, che408IssueSelect)
	w := testutil.Call(t, handler.CreateIssue, req).Want(http.StatusServiceUnavailable)
	che408WantUndetermined(t, w.Body.Bytes(), "description")

	// The refusal has to leave no row behind: a 503 that still created the
	// issue would have dispatched its agent run too.
	var created int
	dbfx.QueryRow(t, `SELECT count(*) FROM issue WHERE parent_issue_id = $1`, parentID).Scan(&created)
	if created != 0 {
		t.Errorf("child issue count = %d, want 0 — the create must not persist when the requirement cannot be determined", created)
	}
}

// TestUpdateComment_IssueLookupErrorFailsClosed covers comment.go's re-fetch of
// the issue the comment belongs to. The comment already exists, so this lookup
// cannot legitimately return "no rows" — any error is a fault, and skipping the
// check on it let an edit strip a citation unchecked.
func TestUpdateComment_IssueLookupErrorFailsClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	che408EnableWorkspacePolicy(t)
	userID, agentID, taskID := che408AgentActor(t, "che408-comment-lookup@multica.test", "che408-comment-lookup")

	issueID := dbfx.Issue(t, "che408 comment lookup failure")
	che408DeclareSource(t, issueID, che408Plan)
	commentID := dbfx.Comment(t, issueID, che408Linked, testutil.Cols{
		"author_type": "agent",
		"author_id":   agentID,
	})

	req := withURLParam(newRequestAs(userID, http.MethodPut, "/api/comments/"+commentID, map[string]any{
		"content": che408Disposable,
	}), "commentId", commentID)
	req = asAgentActor(req, agentID, taskID)

	handler := che408HandlerFailingQuery(t, che408IssueSelect)
	w := testutil.Call(t, handler.UpdateComment, req).Want(http.StatusServiceUnavailable)
	che408WantUndetermined(t, w.Body.Bytes(), "content")

	var content string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE id = $1`, commentID).Scan(&content)
	if content != che408Linked {
		t.Errorf("comment content = %q, want the original citing text — the edit must not land", content)
	}
}
