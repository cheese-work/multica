package handler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

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
