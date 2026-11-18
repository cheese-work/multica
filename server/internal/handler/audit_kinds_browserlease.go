package handler

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for browser leases (CHE-4744, W3-15; plan CHE-4695 F6). One row per
// lease event: start (granted), end (any end reason) and revoke (ended by a revoke).
// The row's own actor columns record who acted. The after snapshot is the whole
// lease, so each row reads alone.
//
// Every field is an id, a class name, a timestamp or an end-reason word. No token,
// token id hash, profile data or page content exists on the lease row, so none can
// reach the audit row.
func init() {
	lease := auditKindSpec{
		TargetType: "browser_lease",
		Allow: []string{
			"agent", "task", "issue", "user", "decision", "profile", "class",
			"start", "end", "end_reason", "revoke_reason",
		},
	}
	for _, kind := range []string{"browser.lease.start", "browser.lease.end", "browser.lease.revoke"} {
		auditKinds[kind] = lease
	}
}

// browserLeaseAuditFields snapshots a lease for one audit kind. start and end are
// RFC 3339 UTC and nil while unset (a queued or still-active lease has no end).
// The reason of an ended lease is revoke_reason on a revoke row and end_reason on
// an end row, so a plain release is never labelled a revoke.
func browserLeaseAuditFields(kind string, l db.BrowserLease) map[string]any {
	fields := map[string]any{
		"agent":    uuidToString(l.AgentID),
		"task":     uuidToString(l.TaskID),
		"issue":    uuidToString(l.IssueID),
		"user":     uuidToString(l.UserID),
		"decision": nil,
		"profile":  uuidToString(l.ProfileID),
		"class":    l.Class,
		"start":    auditTimestamp(l.GrantedAt),
		"end":      auditTimestamp(l.EndedAt),
	}
	if l.DecisionID.Valid {
		fields["decision"] = uuidToString(l.DecisionID)
	}
	switch kind {
	case "browser.lease.revoke":
		fields["revoke_reason"] = auditText(l.EndReason)
	case "browser.lease.end":
		fields["end_reason"] = auditText(l.EndReason)
	}
	return fields
}

// browserLeaseAuditInput builds the audit input for one lease event. The caller
// writes it through insertAuditRow (or writeAudit) in the lease transaction.
func browserLeaseAuditInput(kind string, l db.BrowserLease) auditInput {
	return auditInput{
		Kind:        kind,
		WorkspaceID: l.WorkspaceID,
		TargetID:    l.ID,
		After:       browserLeaseAuditFields(kind, l),
	}
}

func auditTimestamp(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.UTC().Format(time.RFC3339)
}
