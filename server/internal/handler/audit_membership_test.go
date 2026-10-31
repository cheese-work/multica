package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/governance/credential"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2774 L5a: membership, invitation, share link, delegated writes,
// governance config, export privacy and Jev credential audit rows. Each admin
// mutation writes its row in the transaction that applies it; a failing audit
// insert rolls the mutation back and answers a retryable 500.

// failAuditInsertForKind makes the audit insert fail for one kind in one
// workspace. Creates only learn their target id after the insert, so the
// failure is keyed on kind + workspace instead of target.
func failAuditInsertForKind(t *testing.T, kind, workspaceID string) {
	t.Helper()
	name := "audit_fail_kind_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := auditExec(t, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.kind = '%[2]s' AND NEW.workspace_id = '%[3]s' THEN RAISE EXCEPTION 'forced audit insert failure'; END IF;
			RETURN NEW;
		END $$`, name, kind, workspaceID)); err != nil {
		t.Fatalf("create failing function: %v", err)
	}
	if err := auditExec(t, fmt.Sprintf(
		`CREATE TRIGGER %[1]s BEFORE INSERT ON workspace_audit_event FOR EACH ROW EXECUTE FUNCTION %[1]s()`, name)); err != nil {
		t.Fatalf("create failing trigger: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %[1]s ON workspace_audit_event`, name))
		testPool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %[1]s()`, name))
	})
}

// auditMembershipWorkspace builds a workspace owned by the test user, so the
// shared test workspace keeps its members and settings.
func auditMembershipWorkspace(t *testing.T) string {
	t.Helper()
	workspaceID := dbfx.Workspace(t, "audit-membership", "audit-membership-"+uuid.NewString()[:12])
	dbfx.Member(t, workspaceID, testUserID, "owner")
	return workspaceID
}

func requireOneAuditRow(t *testing.T, kind, targetID string) auditRow {
	t.Helper()
	rows := auditRowsFor(t, kind, targetID)
	if len(rows) != 1 {
		t.Fatalf("%s audit rows for %s = %d; want 1", kind, targetID, len(rows))
	}
	row := rows[0]
	if row.Kind != kind {
		t.Errorf("kind = %q; want %q", row.Kind, kind)
	}
	return row
}

func requireAuditFailure(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d (%s); want 500 from the failing audit insert", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("audit failure response must carry Retry-After")
	}
}

func rowText(row auditRow) string {
	encoded, _ := json.Marshal(row)
	return string(encoded)
}

func createAuditInvitation(t *testing.T, workspaceID, email string, inviteeUserID string) string {
	t.Helper()
	params := db.CreateInvitationParams{
		ID:           parseUUID(uuid.NewString()),
		WorkspaceID:  parseUUID(workspaceID),
		InviterID:    parseUUID(testUserID),
		InviteeEmail: email,
		Role:         "member",
		ExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
	}
	if inviteeUserID != "" {
		params.InviteeUserID = parseUUID(inviteeUserID)
	}
	inv, err := testHandler.Queries.CreateInvitation(context.Background(), params)
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	id := uuidToString(inv.ID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace_invitation WHERE id = $1`, id)
	})
	return id
}

func invitationRowCount(t *testing.T, workspaceID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM workspace_invitation WHERE workspace_id = $1`, workspaceID)
}

// --- invitation.create ---

func TestCreateInvitation_WritesAuditRowWithoutEmail(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	email := "audit-invite-" + uuid.NewString()[:8] + "@multica.test"
	req := withURLParam(newRequest(http.MethodPost, "/api/workspaces/"+workspaceID+"/members",
		CreateMemberRequest{Email: email, Role: "admin"}), "id", workspaceID)
	req.Header.Set("X-Actor-Source", "web")
	var created InvitationResponse
	testutil.Call(t, testHandler.CreateInvitation, req).Want(http.StatusCreated).JSON(&created)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace_invitation WHERE id = $1`, created.ID)
	})

	row := requireOneAuditRow(t, "invitation.create", created.ID)
	if row.TargetType != "invitation" || row.ActorType != "member" || row.ActorID != testUserID || row.ActorSource != "web" {
		t.Errorf("row = %+v; want invitation target, member %s via web", row, testUserID)
	}
	if len(row.Before) != 0 {
		t.Errorf("create before = %v; want empty", row.Before)
	}
	if row.After["role"] != "admin" || row.After["status"] != "pending" {
		t.Errorf("after = %v; want role admin, status pending", row.After)
	}
	if strings.Contains(rowText(row), email) {
		t.Errorf("invitee email leaked into the audit row: %s", rowText(row))
	}
}

func TestCreateInvitation_AuditInsertFailureCreatesNoInvitation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	failAuditInsertForKind(t, "invitation.create", workspaceID)
	req := withURLParam(newRequest(http.MethodPost, "/api/workspaces/"+workspaceID+"/members",
		CreateMemberRequest{Email: "audit-invite-fail-" + uuid.NewString()[:8] + "@multica.test", Role: "member"}), "id", workspaceID)
	w := httptest.NewRecorder()
	testHandler.CreateInvitation(w, req)
	requireAuditFailure(t, w)
	if got := invitationRowCount(t, workspaceID); got != 0 {
		t.Fatalf("invitations = %d after failed audit; the create must roll back", got)
	}
}

// --- invitation.revoke ---

func TestRevokeInvitation_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	email := "audit-revoke-" + uuid.NewString()[:8] + "@multica.test"
	invitationID := createAuditInvitation(t, workspaceID, email, "")
	req := withURLParams(newRequest(http.MethodDelete, "/api/workspaces/"+workspaceID+"/invitations/"+invitationID, nil),
		"id", workspaceID, "invitationId", invitationID)
	testutil.Call(t, testHandler.RevokeInvitation, req).Want(http.StatusNoContent)

	row := requireOneAuditRow(t, "invitation.revoke", invitationID)
	if row.TargetType != "invitation" || row.ActorType != "member" || row.ActorID != testUserID {
		t.Errorf("row = %+v; want invitation target by member %s", row, testUserID)
	}
	if row.Before["status"] != "pending" || row.Before["role"] != "member" || len(row.After) != 0 {
		t.Errorf("before=%v after=%v; want the pending invitation removed", row.Before, row.After)
	}
	if strings.Contains(rowText(row), email) {
		t.Errorf("invitee email leaked into the audit row: %s", rowText(row))
	}
}

func TestRevokeInvitation_AuditInsertFailureKeepsInvitation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	invitationID := createAuditInvitation(t, workspaceID, "audit-revoke-fail-"+uuid.NewString()[:8]+"@multica.test", "")
	failAuditInsertFor(t, invitationID)
	req := withURLParams(newRequest(http.MethodDelete, "/api/workspaces/"+workspaceID+"/invitations/"+invitationID, nil),
		"id", workspaceID, "invitationId", invitationID)
	w := httptest.NewRecorder()
	testHandler.RevokeInvitation(w, req)
	requireAuditFailure(t, w)
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace_invitation WHERE id = $1 AND status = 'pending'`, invitationID); got != 1 {
		t.Fatalf("pending invitations = %d after failed audit; the revoke must roll back", got)
	}
}

// --- invitation.accept / invitation.decline ---

func TestAcceptInvitation_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	inviteeID := createEphemeralUser(t, "audit-accept")
	invitationID := createAuditInvitation(t, workspaceID, "audit-accept-"+uuid.NewString()[:8]+"@multica.test", inviteeID)
	req := withURLParam(newRequestAs(inviteeID, http.MethodPost, "/api/invitations/"+invitationID+"/accept", nil), "id", invitationID)
	testutil.Call(t, testHandler.AcceptInvitation, req).Want(http.StatusOK)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, inviteeID)
	})

	row := requireOneAuditRow(t, "invitation.accept", invitationID)
	if row.TargetType != "invitation" || row.ActorType != "member" || row.ActorID != inviteeID {
		t.Errorf("row = %+v; want invitation target by the invitee %s", row, inviteeID)
	}
	if row.Before["status"] != "pending" || row.After["status"] != "accepted" {
		t.Errorf("before=%v after=%v; want pending -> accepted", row.Before, row.After)
	}
}

func TestAcceptInvitation_AuditInsertFailureCreatesNoMember(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	inviteeID := createEphemeralUser(t, "audit-accept-fail")
	invitationID := createAuditInvitation(t, workspaceID, "audit-accept-fail-"+uuid.NewString()[:8]+"@multica.test", inviteeID)
	failAuditInsertFor(t, invitationID)
	req := withURLParam(newRequestAs(inviteeID, http.MethodPost, "/api/invitations/"+invitationID+"/accept", nil), "id", invitationID)
	w := httptest.NewRecorder()
	testHandler.AcceptInvitation(w, req)
	requireAuditFailure(t, w)
	if got := dbfx.Count(t, `SELECT count(*) FROM member WHERE workspace_id = $1 AND user_id = $2`, workspaceID, inviteeID); got != 0 {
		t.Fatalf("member rows = %d after failed audit; the accept must roll back", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace_invitation WHERE id = $1 AND status = 'pending'`, invitationID); got != 1 {
		t.Fatalf("pending invitations = %d after failed audit; want 1", got)
	}
}

func TestDeclineInvitation_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	inviteeID := createEphemeralUser(t, "audit-decline")
	invitationID := createAuditInvitation(t, workspaceID, "audit-decline-"+uuid.NewString()[:8]+"@multica.test", inviteeID)
	req := withURLParam(newRequestAs(inviteeID, http.MethodPost, "/api/invitations/"+invitationID+"/decline", nil), "id", invitationID)
	testutil.Call(t, testHandler.DeclineInvitation, req).Want(http.StatusNoContent)

	row := requireOneAuditRow(t, "invitation.decline", invitationID)
	if row.TargetType != "invitation" || row.ActorType != "member" || row.ActorID != inviteeID {
		t.Errorf("row = %+v; want invitation target by the invitee %s", row, inviteeID)
	}
	if row.Before["status"] != "pending" || row.After["status"] != "declined" {
		t.Errorf("before=%v after=%v; want pending -> declined", row.Before, row.After)
	}
}

func TestDeclineInvitation_AuditInsertFailureKeepsPending(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	inviteeID := createEphemeralUser(t, "audit-decline-fail")
	invitationID := createAuditInvitation(t, workspaceID, "audit-decline-fail-"+uuid.NewString()[:8]+"@multica.test", inviteeID)
	failAuditInsertFor(t, invitationID)
	req := withURLParam(newRequestAs(inviteeID, http.MethodPost, "/api/invitations/"+invitationID+"/decline", nil), "id", invitationID)
	w := httptest.NewRecorder()
	testHandler.DeclineInvitation(w, req)
	requireAuditFailure(t, w)
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace_invitation WHERE id = $1 AND status = 'pending'`, invitationID); got != 1 {
		t.Fatalf("pending invitations = %d after failed audit; the decline must roll back", got)
	}
}

// --- member.update / member.remove / member.leave ---

func TestUpdateMember_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	_, memberID := createEphemeralMember(t, workspaceID, "audit-role", "admin")
	req := withURLParams(newRequest(http.MethodPatch, "/api/workspaces/"+workspaceID+"/members/"+memberID, map[string]any{"role": "member"}),
		"id", workspaceID, "memberId", memberID)
	testutil.Call(t, testHandler.UpdateMember, req).Want(http.StatusOK)

	row := requireOneAuditRow(t, "member.update", memberID)
	if row.TargetType != "member" || row.ActorType != "member" || row.ActorID != testUserID {
		t.Errorf("row = %+v; want member target by %s", row, testUserID)
	}
	if row.Before["role"] != "admin" || row.After["role"] != "member" {
		t.Errorf("before=%v after=%v; want role admin -> member", row.Before, row.After)
	}
}

func TestUpdateMember_AuditInsertFailureKeepsRole(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	_, memberID := createEphemeralMember(t, workspaceID, "audit-role-fail", "admin")
	failAuditInsertFor(t, memberID)
	req := withURLParams(newRequest(http.MethodPatch, "/api/workspaces/"+workspaceID+"/members/"+memberID, map[string]any{"role": "member"}),
		"id", workspaceID, "memberId", memberID)
	w := httptest.NewRecorder()
	testHandler.UpdateMember(w, req)
	requireAuditFailure(t, w)
	var role string
	dbfx.QueryRow(t, `SELECT role FROM member WHERE id = $1`, memberID).Scan(&role)
	if role != "admin" {
		t.Fatalf("role = %q after failed audit; want admin (rolled back)", role)
	}
}

func TestDeleteMember_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	userID, memberID := createEphemeralMember(t, workspaceID, "audit-remove", "admin")
	req := withURLParams(newRequest(http.MethodDelete, "/api/workspaces/"+workspaceID+"/members/"+memberID, nil),
		"id", workspaceID, "memberId", memberID)
	testutil.Call(t, testHandler.DeleteMember, req).Want(http.StatusNoContent)

	row := requireOneAuditRow(t, "member.remove", memberID)
	if row.TargetType != "member" || row.ActorType != "member" || row.ActorID != testUserID {
		t.Errorf("row = %+v; want member target by %s", row, testUserID)
	}
	if row.Before["role"] != "admin" || row.Before["user_id"] != userID || len(row.After) != 0 {
		t.Errorf("before=%v after=%v; want the removed admin %s", row.Before, row.After, userID)
	}
}

func TestDeleteMember_AuditInsertFailureKeepsMember(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	_, memberID := createEphemeralMember(t, workspaceID, "audit-remove-fail", "admin")
	failAuditInsertFor(t, memberID)
	req := withURLParams(newRequest(http.MethodDelete, "/api/workspaces/"+workspaceID+"/members/"+memberID, nil),
		"id", workspaceID, "memberId", memberID)
	w := httptest.NewRecorder()
	testHandler.DeleteMember(w, req)
	requireAuditFailure(t, w)
	if got := dbfx.Count(t, `SELECT count(*) FROM member WHERE id = $1`, memberID); got != 1 {
		t.Fatalf("member rows = %d after failed audit; the removal must roll back", got)
	}
}

func TestLeaveWorkspace_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	userID, memberID := createEphemeralMember(t, workspaceID, "audit-leave", "member")
	req := withURLParam(newRequestAs(userID, http.MethodPost, "/api/workspaces/"+workspaceID+"/leave", nil), "id", workspaceID)
	testutil.Call(t, testHandler.LeaveWorkspace, req).Want(http.StatusNoContent)

	row := requireOneAuditRow(t, "member.leave", memberID)
	if row.TargetType != "member" || row.ActorType != "member" || row.ActorID != userID {
		t.Errorf("row = %+v; want member target by the leaver %s", row, userID)
	}
	if row.Before["role"] != "member" || len(row.After) != 0 {
		t.Errorf("before=%v after=%v; want the leaver's membership removed", row.Before, row.After)
	}
}

func TestLeaveWorkspace_AuditInsertFailureKeepsMember(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	userID, memberID := createEphemeralMember(t, workspaceID, "audit-leave-fail", "member")
	failAuditInsertFor(t, memberID)
	req := withURLParam(newRequestAs(userID, http.MethodPost, "/api/workspaces/"+workspaceID+"/leave", nil), "id", workspaceID)
	w := httptest.NewRecorder()
	testHandler.LeaveWorkspace(w, req)
	requireAuditFailure(t, w)
	if got := dbfx.Count(t, `SELECT count(*) FROM member WHERE id = $1`, memberID); got != 1 {
		t.Fatalf("member rows = %d after failed audit; the leave must roll back", got)
	}
}

// --- share_link.create ---

func TestCreateShareLink_WritesAuditRowWithoutCode(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	req := withURLParam(newRequest(http.MethodPost, "/api/workspaces/"+workspaceID+"/share-links",
		CreateShareLinkRequest{Role: "admin", MaxUses: ptrInt(5)}), "id", workspaceID)
	var created ShareLinkResponse
	testutil.Call(t, testHandler.CreateShareLink, req).Want(http.StatusCreated).JSON(&created)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace_share_link WHERE id = $1`, created.ID)
	})
	if created.Code == "" {
		t.Fatal("setup: response carried no code")
	}

	row := requireOneAuditRow(t, "share_link.create", created.ID)
	if row.TargetType != "share_link" || row.ActorType != "member" || row.ActorID != testUserID {
		t.Errorf("row = %+v; want share_link target by %s", row, testUserID)
	}
	if row.After["role"] != "admin" || row.After["max_uses"] != float64(5) || row.After["is_active"] != true {
		t.Errorf("after = %v; want role admin, max_uses 5, is_active true", row.After)
	}
	if strings.Contains(rowText(row), created.Code) {
		t.Errorf("share link code leaked into the audit row: %s", rowText(row))
	}
}

func TestCreateShareLink_AuditInsertFailureKeepsOldLink(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	workspaceID := auditMembershipWorkspace(t)
	old := createTestShareLink(t, workspaceID, "member", 0, 0)
	failAuditInsertForKind(t, "share_link.create", workspaceID)
	req := withURLParam(newRequest(http.MethodPost, "/api/workspaces/"+workspaceID+"/share-links",
		CreateShareLinkRequest{Role: "member"}), "id", workspaceID)
	w := httptest.NewRecorder()
	testHandler.CreateShareLink(w, req)
	requireAuditFailure(t, w)
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace_share_link WHERE workspace_id = $1`, workspaceID); got != 1 {
		t.Fatalf("share links = %d after failed audit; want only the old one", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace_share_link WHERE id = $1 AND is_active`, uuidToString(old.ID)); got != 1 {
		t.Fatalf("old link must stay active after the rolled-back rotation")
	}
}

// --- workspace.delegated_writes.update ---

func TestUpdateDelegatedWrites_WritesAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	wsID, agentID := delegatedWritesFixture(t, "audit-delegated-"+uuid.NewString()[:8])
	putDelegatedWrites(t, wsID, map[string]any{
		"allowed_agents":          []string{agentID},
		"allowed_fields":          []string{"workspace.context", "agent.instructions"},
		"requires_human_decision": false,
	}, nil).Want(http.StatusOK)

	row := requireOneAuditRow(t, "workspace.delegated_writes.update", wsID)
	if row.TargetType != "workspace" || row.ActorType != "member" || row.ActorID != testUserID {
		t.Errorf("row = %+v; want workspace target by %s", row, testUserID)
	}
	if row.After["allowed_agents"] != agentID || row.After["allowed_fields"] != "agent.instructions,workspace.context" {
		t.Errorf("after = %v; want the sorted agent and field lists", row.After)
	}
	if row.Before["requires_human_decision"] != true || row.After["requires_human_decision"] != false {
		t.Errorf("before=%v after=%v; want requires_human_decision true -> false", row.Before, row.After)
	}
}

func TestUpdateDelegatedWrites_AuditInsertFailureKeepsSetting(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	wsID, agentID := delegatedWritesFixture(t, "audit-delegated-fail-"+uuid.NewString()[:8])
	failAuditInsertFor(t, wsID)
	req := withURLParam(newRequest("PUT", "/api/workspaces/"+wsID+"/delegated-writes", map[string]any{
		"allowed_agents": []string{agentID}, "allowed_fields": []string{"workspace.context"},
	}), "id", wsID)
	w := httptest.NewRecorder()
	testHandler.UpdateDelegatedWrites(w, req)
	requireAuditFailure(t, w)
	if _, ok := delegatedWriteCheck(t, wsID, agentID, "workspace.context"); ok {
		t.Fatal("delegated write allowed after a failed audit; the setting must roll back")
	}
}

// --- governance.config.update ---

func TestPatchGovernanceConfig_WritesAuditRowWithoutURL(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceConfigFixture(t)
	t.Cleanup(func() { resetGovernanceConfigFixture(t) })
	const secretURL = "https://user:hunter2@jev.example.test/v1"
	body := []byte(`{"expected_version":0,"request_id":"` + uuid.NewString() + `","jev_governance_enabled":true,"jev_api_url":"` + secretURL + `","limits":{"max_refreshes":4}}`)
	before := len(auditRowsFor(t, "governance.config.update", testWorkspaceID))
	if w := invokeGovernanceConfig(t, testUserID, http.MethodPatch, body, ""); w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", w.Code, w.Body.String())
	}

	rows := auditRowsFor(t, "governance.config.update", testWorkspaceID)
	if len(rows) != before+1 {
		t.Fatalf("governance.config.update rows = %d; want %d", len(rows), before+1)
	}
	row := rows[len(rows)-1]
	if row.TargetType != "workspace" || row.ActorType != "member" || row.ActorID != testUserID {
		t.Errorf("row = %+v; want workspace target by %s", row, testUserID)
	}
	if row.After["jev_governance_enabled"] != true || row.After["max_refreshes"] != float64(4) || row.After["config_version"] != float64(1) {
		t.Errorf("after = %v; want governance enabled, max_refreshes 4, version 1", row.After)
	}
	if row.After["jev_api_url"] != "changed" {
		t.Errorf("jev_api_url = %v; want only the changed marker", row.After["jev_api_url"])
	}
	if strings.Contains(rowText(row), "hunter2") {
		t.Errorf("URL credentials leaked into the audit row: %s", rowText(row))
	}
}

func TestPatchGovernanceConfig_AuditInsertFailureKeepsConfig(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	resetGovernanceConfigFixture(t)
	t.Cleanup(func() { resetGovernanceConfigFixture(t) })
	failAuditInsertFor(t, testWorkspaceID)
	body := []byte(`{"expected_version":0,"request_id":"` + uuid.NewString() + `","jev_governance_enabled":true}`)
	w := invokeGovernanceConfig(t, testUserID, http.MethodPatch, body, "")
	requireAuditFailure(t, w)
	if got := dbfx.Count(t, `SELECT count(*) FROM governance_workspace_config WHERE workspace_id = $1 AND config_version > 0`, testWorkspaceID); got != 0 {
		t.Fatalf("config versions advanced = %d after failed audit; want rolled back", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM governance_workspace_config_audit WHERE workspace_id = $1`, testWorkspaceID); got != 0 {
		t.Fatalf("governance config audit rows = %d after failed audit; want 0", got)
	}
}

// --- workspace.export_privacy.update ---

func TestUpdateWorkspaceExportPrivacy_WritesAuditRow(t *testing.T) {
	requireProvenanceDB(t)
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })
	before := len(auditRowsFor(t, "workspace.export_privacy.update", testWorkspaceID))
	testutil.Call(t, testHandler.UpdateWorkspaceExportPrivacy,
		exportPrivacyRequest(http.MethodPatch, testWorkspaceID, testUserID, map[string]any{"manifest_retention_days": 30})).
		Want(http.StatusOK)

	rows := auditRowsFor(t, "workspace.export_privacy.update", testWorkspaceID)
	if len(rows) != before+1 {
		t.Fatalf("workspace.export_privacy.update rows = %d; want %d", len(rows), before+1)
	}
	row := rows[len(rows)-1]
	if row.TargetType != "workspace" || row.ActorType != "member" || row.ActorID != testUserID {
		t.Errorf("row = %+v; want workspace target by %s", row, testUserID)
	}
	if row.Before["export_manifest_retention_days"] != float64(90) || row.After["export_manifest_retention_days"] != float64(30) {
		t.Errorf("before=%v after=%v; want retention 90 -> 30", row.Before, row.After)
	}
}

func TestUpdateWorkspaceExportPrivacy_AuditInsertFailureKeepsPolicy(t *testing.T) {
	requireProvenanceDB(t)
	resetExportPrivacy(t, testWorkspaceID)
	t.Cleanup(func() { resetExportPrivacy(t, testWorkspaceID) })
	failAuditInsertFor(t, testWorkspaceID)
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspaceExportPrivacy(w,
		exportPrivacyRequest(http.MethodPatch, testWorkspaceID, testUserID, map[string]any{"manifest_retention_days": 30}))
	requireAuditFailure(t, w)
	var days int
	dbfx.QueryRow(t, `SELECT export_manifest_retention_days FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&days)
	if days != 90 {
		t.Fatalf("retention days = %d after failed audit; want 90 (rolled back)", days)
	}
}

// --- jev.credential.* ---

func jevAuditFixture(t *testing.T) (string, Handler) {
	t.Helper()
	if testHandler == nil || testPool == nil || dbfx == nil {
		t.Skip("handler test database is unavailable")
	}
	requireJevCredentialTable(t)
	workspaceID := auditMembershipWorkspace(t)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM governance_jev_credential WHERE workspace_id = $1`, workspaceID)
	})
	ring, err := credential.NewKeyring(credential.Key{ID: "current", Material: bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	h := *testHandler
	h.JevCredentials = ring
	return workspaceID, h
}

func jevCredentialStored(t *testing.T, workspaceID string) bool {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM governance_jev_credential WHERE workspace_id = $1`, workspaceID) == 1
}

func TestPutJevCredential_AuditsSetAndRotateWithoutValue(t *testing.T) {
	workspaceID, h := jevAuditFixture(t)
	put := func(key string) *httptest.ResponseRecorder {
		return invokeJevCredentialForWorkspace(t, &h, workspaceID, testUserID, http.MethodPut, []byte(`{"api_key":"`+key+`"}`), "")
	}
	firstKey, secondKey := jevCredentialTestSecret+"-first", jevCredentialTestSecret+"-second"
	if w := put(firstKey); w.Code != http.StatusOK {
		t.Fatalf("first PUT = %d: %s", w.Code, w.Body.String())
	}
	if w := put(secondKey); w.Code != http.StatusOK {
		t.Fatalf("second PUT = %d: %s", w.Code, w.Body.String())
	}

	set := requireOneAuditRow(t, "jev.credential.set", workspaceID)
	rotate := requireOneAuditRow(t, "jev.credential.rotate", workspaceID)
	for _, row := range []auditRow{set, rotate} {
		if row.TargetType != "workspace" || row.ActorType != "member" || row.ActorID != testUserID {
			t.Errorf("row = %+v; want workspace target by %s", row, testUserID)
		}
		for _, leaked := range []string{firstKey, secondKey, jevCredentialTestSecret} {
			if strings.Contains(rowText(row), leaked) {
				t.Errorf("credential value leaked into the audit row: %s", rowText(row))
			}
		}
	}
}

func TestPutJevCredential_AuditInsertFailureStoresNothing(t *testing.T) {
	workspaceID, h := jevAuditFixture(t)
	failAuditInsertFor(t, workspaceID)
	w := invokeJevCredentialForWorkspace(t, &h, workspaceID, testUserID, http.MethodPut, []byte(`{"api_key":"`+jevCredentialTestSecret+`"}`), "")
	requireAuditFailure(t, w)
	if jevCredentialStored(t, workspaceID) {
		t.Fatal("credential stored although the audit insert failed; the write must roll back")
	}
}

func TestDeleteJevCredential_WritesAuditRow(t *testing.T) {
	workspaceID, h := jevAuditFixture(t)
	if w := invokeJevCredentialForWorkspace(t, &h, workspaceID, testUserID, http.MethodPut, []byte(`{"api_key":"`+jevCredentialTestSecret+`"}`), ""); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}
	if w := invokeJevCredentialForWorkspace(t, &h, workspaceID, testUserID, http.MethodDelete, nil, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body.String())
	}
	row := requireOneAuditRow(t, "jev.credential.delete", workspaceID)
	if row.TargetType != "workspace" || row.ActorType != "member" || row.ActorID != testUserID {
		t.Errorf("row = %+v; want workspace target by %s", row, testUserID)
	}
	if strings.Contains(rowText(row), jevCredentialTestSecret) {
		t.Errorf("credential value leaked into the audit row: %s", rowText(row))
	}
}

func TestDeleteJevCredential_AuditInsertFailureKeepsCredential(t *testing.T) {
	workspaceID, h := jevAuditFixture(t)
	if w := invokeJevCredentialForWorkspace(t, &h, workspaceID, testUserID, http.MethodPut, []byte(`{"api_key":"`+jevCredentialTestSecret+`"}`), ""); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}
	failAuditInsertForKind(t, "jev.credential.delete", workspaceID)
	w := invokeJevCredentialForWorkspace(t, &h, workspaceID, testUserID, http.MethodDelete, nil, "")
	requireAuditFailure(t, w)
	if !jevCredentialStored(t, workspaceID) {
		t.Fatal("credential deleted although the audit insert failed; the delete must roll back")
	}
}

func ptrInt(v int) *int { return &v }
