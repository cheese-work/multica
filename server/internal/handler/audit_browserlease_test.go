package handler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"net/http"

	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-4744 (W3-15, plan CHE-4695 F6): browser lease events are admin audit kinds.
// Each row carries actor (the audit row's own actor columns), agent, task, issue,
// user, decision, profile, class, start, end and the revoke reason.

var browserLeaseAuditKinds = []string{
	"browser.lease.start", "browser.lease.end", "browser.lease.revoke",
}

func browserLeaseForAudit(t *testing.T, withDecision bool) db.BrowserLease {
	t.Helper()
	granted := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	ended := granted.Add(10 * time.Minute)
	l := db.BrowserLease{
		ID:          parseUUID(uuid.NewString()),
		WorkspaceID: parseUUID(testWorkspaceID),
		InstanceID:  parseUUID(uuid.NewString()),
		ProfileID:   parseUUID(uuid.NewString()),
		AgentID:     parseUUID(uuid.NewString()),
		TaskID:      parseUUID(uuid.NewString()),
		IssueID:     parseUUID(uuid.NewString()),
		UserID:      parseUUID(uuid.NewString()),
		Class:       "S2",
		State:       "ended",
		GrantedAt:   pgtype.Timestamptz{Time: granted, Valid: true},
		EndedAt:     pgtype.Timestamptz{Time: ended, Valid: true},
		EndReason:   pgtype.Text{String: "plugin_disabled", Valid: true},
	}
	if withDecision {
		l.DecisionID = parseUUID(uuid.NewString())
	}
	return l
}

func TestBrowserLeaseAuditKindsRegistered(t *testing.T) {
	for _, kind := range browserLeaseAuditKinds {
		if !AuditKindRegistered(kind) {
			t.Errorf("audit kind %q is not registered", kind)
		}
		if got := auditKinds[kind].TargetType; got != "browser_lease" {
			t.Errorf("kind %q target type = %q, want browser_lease", kind, got)
		}
	}
}

func TestBrowserLeaseAuditFields(t *testing.T) {
	l := browserLeaseForAudit(t, true)
	got := browserLeaseAuditFields("browser.lease.revoke", l)
	want := map[string]any{
		"agent":         uuidToString(l.AgentID),
		"task":          uuidToString(l.TaskID),
		"issue":         uuidToString(l.IssueID),
		"user":          uuidToString(l.UserID),
		"decision":      uuidToString(l.DecisionID),
		"profile":       uuidToString(l.ProfileID),
		"class":         "S2",
		"start":         "2026-11-18T06:00:00Z",
		"end":           "2026-11-18T06:10:00Z",
		"revoke_reason": "plugin_disabled",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %q = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["end_reason"]; ok {
		t.Error("revoke kind must not carry end_reason")
	}

	end := browserLeaseAuditFields("browser.lease.end", l)
	if end["end_reason"] != "plugin_disabled" {
		t.Errorf("end kind end_reason = %v", end["end_reason"])
	}
	if _, ok := end["revoke_reason"]; ok {
		t.Error("end kind must not carry revoke_reason")
	}

	start := browserLeaseAuditFields("browser.lease.start", browserLeaseForAudit(t, false))
	if start["decision"] != nil {
		t.Errorf("no decision: field = %v, want nil", start["decision"])
	}
}

func TestBrowserLeaseAuditRowStoresAllFields(t *testing.T) {
	for _, kind := range browserLeaseAuditKinds {
		t.Run(kind, func(t *testing.T) {
			l := browserLeaseForAudit(t, true)
			actorID := uuid.NewString()
			taskID := uuid.NewString()
			actor := auditActor{Type: "agent", ID: parseUUID(actorID), TaskID: parseUUID(taskID), Source: "daemon"}
			ctx := context.Background()
			if err := insertAuditRow(ctx, testHandler.Queries, actor, browserLeaseAuditInput(kind, l)); err != nil {
				t.Fatalf("insert: %v", err)
			}
			rows := auditRowsFor(t, kind, uuidToString(l.ID))
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			row := rows[0]
			if row.ActorType != "agent" || row.ActorID != actorID || row.TaskID != taskID {
				t.Errorf("actor = %s/%s task %s", row.ActorType, row.ActorID, row.TaskID)
			}
			if row.TargetType != "browser_lease" {
				t.Errorf("target type = %q", row.TargetType)
			}
			b, _ := json.Marshal(row.After)
			for _, key := range []string{"agent", "task", "issue", "user", "decision", "profile", "class", "start", "end"} {
				if v, ok := row.After[key]; !ok || v == auditChangedMarker || v == nil {
					t.Errorf("after[%q] = %v (row %s)", key, v, b)
				}
			}
			if row.After["agent"] != uuidToString(l.AgentID) {
				t.Errorf("agent id stored as %v", row.After["agent"])
			}
		})
	}
}

// Lease transitions write their audit rows (CHE-4744 P1 fix): each test performs the
// transition through the real handler and service, then reads the stored audit row.

func TestBrowserLeaseAuditKindsMatchTheServiceConstants(t *testing.T) {
	for kind, want := range map[string]string{
		browserlease.AuditStart: "browser.lease.start", browserlease.AuditEnd: "browser.lease.end", browserlease.AuditRevoke: "browser.lease.revoke",
	} {
		if kind != want || !AuditKindRegistered(kind) {
			t.Errorf("service kind %q: want %q registered", kind, want)
		}
	}
}

func TestBrowserLeaseTransitionsWriteAuditRowsWithActor(t *testing.T) {
	f := newBrowserLeaseFixture(t, "running")
	f.h.BrowserLeases.Audit = BrowserLeaseAudit
	prevTx := f.h.TxStarter
	t.Cleanup(func() { f.h.TxStarter = prevTx })

	w := f.call(t, f.h.MintBrowserLease, "/x", map[string]string{"profile_id": f.profile})
	if w.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	var minted struct {
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}

	assertRow := func(kind, reasonKey, reason string) {
		t.Helper()
		rows := auditRowsFor(t, kind, minted.LeaseID)
		if len(rows) != 1 {
			t.Fatalf("%s rows = %d, want 1", kind, len(rows))
		}
		row := rows[0]
		if row.ActorType != "agent" || row.ActorID != f.agentID || row.TaskID != f.taskID {
			t.Errorf("%s actor = %s/%s task %s, want agent %s task %s", kind, row.ActorType, row.ActorID, row.TaskID, f.agentID, f.taskID)
		}
		if row.TargetType != "browser_lease" {
			t.Errorf("%s target type = %q", kind, row.TargetType)
		}
		for key, want := range map[string]any{"agent": f.agentID, "task": f.taskID, "profile": f.profile, "class": "S0", "user": testUserID} {
			if row.After[key] != want {
				t.Errorf("%s after[%s] = %v, want %v", kind, key, row.After[key], want)
			}
		}
		for _, key := range []string{"issue", "start"} {
			if v, ok := row.After[key]; !ok || v == nil {
				t.Errorf("%s after[%s] missing", kind, key)
			}
		}
		if reasonKey != "" && row.After[reasonKey] != reason {
			t.Errorf("%s after[%s] = %v, want %s", kind, reasonKey, row.After[reasonKey], reason)
		}
	}
	assertRow("browser.lease.start", "", "")

	// An agent release goes through the revoker: a revoke row, with the agent as actor.
	f.h.TxStarter = testPool
	if w := f.call(t, f.h.ReleaseBrowserLease, "/x", nil, "leaseId", minted.LeaseID); w.Code != http.StatusOK {
		t.Fatalf("release: %d %s", w.Code, w.Body.String())
	}
	assertRow("browser.lease.revoke", "revoke_reason", browserlease.ReasonRelease)
}

func TestBrowserLeaseExpiryWritesAnEndRow(t *testing.T) {
	f := newBrowserLeaseFixture(t, "running")
	f.h.BrowserLeases.Audit = BrowserLeaseAudit
	w := f.call(t, f.h.MintBrowserLease, "/x", map[string]string{"profile_id": f.profile})
	var minted struct {
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	f.h.BrowserLeases.Now = func() time.Time { return time.Now().Add(browserlease.MaxLeaseDuration) }
	if w := f.call(t, f.h.RenewBrowserLease, "/x", nil, "leaseId", minted.LeaseID); w.Code != http.StatusConflict {
		t.Fatalf("renew past the hard limit: %d %s", w.Code, w.Body.String())
	}
	rows := auditRowsFor(t, "browser.lease.end", minted.LeaseID)
	if len(rows) != 1 || rows[0].ActorType != "agent" || rows[0].ActorID != f.agentID || rows[0].After["end_reason"] != "expired" {
		t.Fatalf("end rows = %+v, want one agent row with end_reason expired", rows)
	}
	if _, ok := rows[0].After["end"]; !ok {
		t.Error("end row has no end time")
	}
}

func TestBrowserLeaseKillSwitchAndPluginOffAuditTheirActors(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "CHE-4744 audit runtime")
	agentID := dbfx.Agent(t, "CHE-4744 audit agent", runtimeID)
	issueID := dbfx.Issue(t, "CHE-4744 audit issue")
	task := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
	prevTx := testHandler.TxStarter
	testHandler.TxStarter = testPool
	t.Cleanup(func() { testHandler.TxStarter = prevTx })

	killed := seedActiveBrowserLease(t, task)
	req := withURLParam(newRequest(http.MethodPost, "/x", nil), "id", testWorkspaceID)
	testutil.Call(t, testHandler.RevokeAllBrowserLeases, req).Want(http.StatusOK)
	rows := auditRowsFor(t, "browser.lease.revoke", killed)
	if len(rows) != 1 || rows[0].ActorType != "member" || rows[0].ActorID != testUserID || rows[0].After["revoke_reason"] != browserlease.ReasonRevokeAll {
		t.Fatalf("kill-switch rows = %+v, want one member row (%s) with revoke_all", rows, testUserID)
	}

	off := seedActiveBrowserLease(t, task)
	testHandler.revokeLeasesOfDisabledPlugin(newRequest(http.MethodPost, "/x", nil), db.PluginInstallation{PluginKey: SteelBrowserPluginKey, WorkspaceID: parseUUID(testWorkspaceID)})
	rows = auditRowsFor(t, "browser.lease.revoke", off)
	if len(rows) != 1 || rows[0].After["revoke_reason"] != browserlease.ReasonPluginDisabled {
		t.Fatalf("plugin-off rows = %+v, want one revoke row with plugin_disabled", rows)
	}
}
