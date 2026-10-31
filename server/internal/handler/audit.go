package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Admin audit trail (CHE-2774). Every admin mutation calls writeAudit inside
// the transaction that applies it, so the row commits or rolls back with the
// change. There is no after-commit or best-effort path: when the insert fails
// the caller rolls back and answers 500 through writeAuditFailure.
//
// The row records the request, never the outcome of a step outside the
// database. Steps the handler runs after commit (daemon notices, update
// requests) are listed in auditInput.External.

// Limit: the append-only trigger guards against bugs and accidents only. Every
// app session is a Postgres superuser and can set multica.audit_purge or drop
// the trigger, so the trail is not tamper-proof. There is no hash chain.

// External step kinds recorded in workspace_audit_event.external_requested.
const (
	auditExternalRuntimeGone  = "runtime_gone_notice"
	auditExternalDaemonUpdate = "daemon_update_request"
)

// auditKindSpec declares what a kind may record. Redaction is default-deny:
// a changed field outside Allow and Text is stored as the key name plus the
// marker "changed", never its value.
type auditKindSpec struct {
	TargetType string
	// Allow lists fields whose scalar values are not secret and may be stored.
	Allow []string
	// Text lists long-text fields (instructions, context). Stored as length,
	// hash, line count and a line diff stat, never the text.
	Text []string
}

const auditChangedMarker = "changed"

// auditKinds is the registry of audit kinds. A kind missing here fails the
// write, so a typo cannot silently drop an audit row.
var auditKinds = map[string]auditKindSpec{
	"workspace.update": {
		TargetType: "workspace",
		Allow:      []string{"name", "issue_prefix"},
		Text:       []string{"description", "context", "compact_prompt"},
	},
	"agent.update": {
		TargetType: "agent",
		Allow: []string{
			"name", "model", "runtime_id", "status", "max_concurrent_tasks",
			"thinking_level", "service_tier", "visibility", "permission_mode",
		},
		Text: []string{"description", "instructions"},
	},
	// Env values never reach the audit row: no field is allowed, so the row
	// holds the changed key names and the marker only.
	"agent.env.update": {TargetType: "agent"},
	"runtime.delete": {
		TargetType: "runtime",
		Allow:      []string{"name", "provider", "runtime_mode", "visibility", "status"},
	},
	"audit.retention_purge": {TargetType: "workspace"},
}

// auditActor is who a row is attributed to.
type auditActor struct {
	Type   string // member | agent | system
	ID     pgtype.UUID
	TaskID pgtype.UUID
	Source string
}

// auditSystemActor attributes scheduler and boot jobs.
var auditSystemActor = auditActor{Type: "system", Source: "system"}

// auditActorFromRequest reuses resolveActor. A member session and a member PAT
// are both "member"; an agent task token is "agent" plus the task it ran in.
func (h *Handler) auditActorFromRequest(r *http.Request, workspaceID pgtype.UUID) auditActor {
	actorType, actorID := h.resolveActor(r, requestUserID(r), uuidToString(workspaceID))
	actor := auditActor{
		Type:   actorType,
		ID:     optionalActorUUID(actorID),
		Source: r.Header.Get("X-Actor-Source"),
	}
	if actorType == "agent" {
		actor.TaskID = optionalActorUUID(r.Header.Get("X-Task-ID"))
	}
	return actor
}

// auditInput describes one mutation. Before and After hold the raw field
// values; writeAudit redacts them per the kind's spec before storing.
type auditInput struct {
	Kind        string
	WorkspaceID pgtype.UUID
	TargetID    pgtype.UUID
	Before      map[string]any
	After       map[string]any
	External    []string
	// Actor overrides the request actor (system jobs).
	Actor *auditActor
}

// writeAudit inserts the audit row through q, which must be bound to the
// mutation's transaction.
func (h *Handler) writeAudit(ctx context.Context, q *db.Queries, r *http.Request, in auditInput) error {
	actor := auditSystemActor
	switch {
	case in.Actor != nil:
		actor = *in.Actor
	case r != nil:
		actor = h.auditActorFromRequest(r, in.WorkspaceID)
	}
	return insertAuditRow(ctx, q, actor, in)
}

func insertAuditRow(ctx context.Context, q *db.Queries, actor auditActor, in auditInput) error {
	spec, ok := auditKinds[in.Kind]
	if !ok {
		return fmt.Errorf("audit: unknown kind %q", in.Kind)
	}
	before, after := redactAuditChange(spec, in.Before, in.After)
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		return fmt.Errorf("audit: encode before: %w", err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		return fmt.Errorf("audit: encode after: %w", err)
	}
	external := in.External
	if external == nil {
		external = []string{}
	}
	return q.InsertWorkspaceAuditEvent(ctx, db.InsertWorkspaceAuditEventParams{
		ID:                dbid.NewV7(),
		WorkspaceID:       in.WorkspaceID,
		ActorType:         actor.Type,
		ActorID:           actor.ID,
		TaskID:            actor.TaskID,
		ActorSource:       actor.Source,
		Kind:              in.Kind,
		TargetType:        spec.TargetType,
		TargetID:          in.TargetID,
		Before:            beforeJSON,
		After:             afterJSON,
		ExternalRequested: external,
	})
}

// auditWriteError marks a failed audit insert inside a helper that returns a
// plain error, so the handler can tell it from the mutation's own failure.
type auditWriteError struct{ err error }

func (e auditWriteError) Error() string { return "audit write: " + e.err.Error() }
func (e auditWriteError) Unwrap() error { return e.err }

// failedAudit answers 500 and returns true when err is an auditWriteError.
func failedAudit(w http.ResponseWriter, r *http.Request, err error, kind string) bool {
	var ae auditWriteError
	if !errors.As(err, &ae) {
		return false
	}
	writeAuditFailure(w, r, ae.err, kind)
	return true
}

// writeAuditFailure answers a failed audit insert. The caller has already
// returned from the transaction without committing, so nothing was applied
// and the request can be retried.
func writeAuditFailure(w http.ResponseWriter, r *http.Request, err error, kind string) {
	slog.Error("audit write failed; mutation rolled back",
		append(logger.RequestAttrs(r), "error", err, "audit_kind", kind)...)
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusInternalServerError, "audit log write failed; nothing was changed, retry the request")
}

// redactAuditChange returns the stored before/after summaries: only fields that
// differ appear, with allowlisted scalars as values, long text as a digest
// and everything else as the changed marker.
func redactAuditChange(spec auditKindSpec, before, after map[string]any) (map[string]any, map[string]any) {
	outBefore, outAfter := map[string]any{}, map[string]any{}
	for key := range unionKeys(before, after) {
		oldVal, hadOld := before[key]
		newVal, hasNew := after[key]
		if hadOld && hasNew && reflect.DeepEqual(oldVal, newVal) {
			continue
		}
		switch {
		case auditListHas(spec.Text, key):
			oldText, newText := asText(oldVal), asText(newVal)
			if hadOld {
				outBefore[key] = textDigest(oldText)
			}
			if hasNew {
				digest := textDigest(newText)
				digest["lines_added"], digest["lines_removed"] = lineDiffStat(oldText, newText)
				outAfter[key] = digest
			}
		case auditListHas(spec.Allow, key):
			if hadOld {
				outBefore[key] = allowedScalar(oldVal)
			}
			if hasNew {
				outAfter[key] = allowedScalar(newVal)
			}
		default:
			if hadOld {
				outBefore[key] = auditChangedMarker
			}
			if hasNew {
				outAfter[key] = auditChangedMarker
			}
		}
	}
	return outBefore, outAfter
}

// allowedScalar keeps strings, numbers and booleans; any other shape on an
// allowlisted key (a map or slice that could carry a secret) degrades to the marker.
func allowedScalar(v any) any {
	switch t := v.(type) {
	case nil, bool, int, int32, int64, float64:
		return t
	case string:
		if len(t) > 200 {
			return t[:200]
		}
		return t
	case pgtype.UUID:
		return uuidToString(t)
	}
	return auditChangedMarker
}

func asText(v any) string {
	s, _ := v.(string)
	return s
}

func textDigest(s string) map[string]any {
	sum := sha256.Sum256([]byte(s))
	lines := 0
	if s != "" {
		lines = strings.Count(s, "\n") + 1
	}
	return map[string]any{"len": len(s), "sha256": hex.EncodeToString(sum[:]), "lines": lines}
}

// lineDiffStat counts lines present in next but not prev, and the reverse, as
// multisets. It is a size signal for the audit page, not a patch.
func lineDiffStat(prev, next string) (added, removed int) {
	counts := map[string]int{}
	if prev != "" {
		for _, line := range strings.Split(prev, "\n") {
			counts[line]++
		}
	}
	if next != "" {
		for _, line := range strings.Split(next, "\n") {
			if counts[line] > 0 {
				counts[line]--
			} else {
				added++
			}
		}
	}
	for _, left := range counts {
		removed += left
	}
	return added, removed
}

func unionKeys(a, b map[string]any) map[string]struct{} {
	keys := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}

func auditListHas(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// auditText flattens a nullable text column for a field snapshot.
func auditText(t pgtype.Text) any {
	if !t.Valid {
		return nil
	}
	return t.String
}

// workspaceAuditFields snapshots the workspace columns an admin can change.
// Settings and repos go in as text so a change shows up as a diff; the kind's
// allowlist then keeps their values out of the stored row.
func workspaceAuditFields(ws db.Workspace) map[string]any {
	return map[string]any{
		"name":         ws.Name,
		"description":  auditText(ws.Description),
		"context":      auditText(ws.Context),
		"issue_prefix": ws.IssuePrefix,
		"avatar_url":   auditText(ws.AvatarUrl),
		"settings":     string(ws.Settings),
		"repos":        string(ws.Repos),
	}
}

// agentAuditFields snapshots the agent columns an admin can change. Env, args
// and MCP config are snapshotted as text for the diff only: none is allowlisted.
func agentAuditFields(a db.Agent) map[string]any {
	return map[string]any{
		"name":                 a.Name,
		"description":          a.Description,
		"instructions":         a.Instructions,
		"runtime_id":           a.RuntimeID,
		"model":                auditText(a.Model),
		"thinking_level":       auditText(a.ThinkingLevel),
		"service_tier":         auditText(a.ServiceTier),
		"visibility":           a.Visibility,
		"permission_mode":      a.PermissionMode,
		"status":               a.Status,
		"max_concurrent_tasks": a.MaxConcurrentTasks,
		"avatar_url":           auditText(a.AvatarUrl),
		"custom_args":          string(a.CustomArgs),
		"mcp_config":           string(a.McpConfig),
	}
}

// runtimeAuditFields snapshots a runtime for delete. Metadata and device info
// are left out: they can carry host details and are not needed to read the row.
func runtimeAuditFields(rt db.AgentRuntime) map[string]any {
	return map[string]any{
		"name": rt.Name, "provider": rt.Provider, "runtime_mode": rt.RuntimeMode,
		"visibility": rt.Visibility, "status": rt.Status,
	}
}

// envAuditFields lifts an env map into a snapshot. Values are present only so
// a changed value shows up as a diff; no env key is ever allowlisted.
func envAuditFields(env map[string]string) map[string]any {
	out := make(map[string]any, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// AuditKindRegistered reports whether kind has a redaction spec. The route
// guard test in cmd/server uses it so a route cannot claim a kind that would
// fail at write time.
func AuditKindRegistered(kind string) bool {
	_, ok := auditKinds[kind]
	return ok
}
