package handler

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for runtime pause/update/local-skill import, skill labels and
// project wakeup definitions and GitHub route grants (CHE-5647, CHE-4807 E2).
// Skill file writes reuse skill.update. A field not listed in Allow or Text is
// stored as the changed marker, so no config body or request text is kept.
func init() {
	for _, kind := range []string{"runtime.pause", "runtime.resume"} {
		auditKinds[kind] = auditKindSpec{TargetType: "runtime", Allow: []string{"paused", "reason"}}
	}
	auditKinds["runtime.update"] = auditKindSpec{TargetType: "runtime", Allow: []string{"target_version"}}
	// name and description are request text: digest only.
	auditKinds["runtime.local_skill.import"] = auditKindSpec{
		TargetType: "runtime",
		Allow:      []string{"skill_key", "action", "target_skill_id"},
		Text:       []string{"name", "description"},
	}
	for _, kind := range []string{"skill.label.attach", "skill.label.detach"} {
		auditKinds[kind] = auditKindSpec{TargetType: "skill", Allow: []string{"label_ids"}}
	}
	// config is the rule body (instructions, filters): never allowlisted.
	for _, kind := range []string{"project.wakeup_definition.create", "project.wakeup_definition.update", "project.wakeup_definition.delete"} {
		auditKinds[kind] = auditKindSpec{TargetType: "project", Allow: []string{"rule_key", "revision"}}
	}
	for _, kind := range []string{"project.route_grant.set", "project.route_grant.remove"} {
		auditKinds[kind] = auditKindSpec{TargetType: "project", Allow: []string{"route_grant_user_id"}}
	}
}

// pauseAuditFields snapshots a machine pause; a zero pause means not paused.
func pauseAuditFields(p *db.DaemonPause) map[string]any {
	if p == nil {
		return map[string]any{"paused": false}
	}
	return map[string]any{"paused": true, "reason": p.Reason}
}

// routeGrantAuditFields snapshots a GitHub route grant for one user.
func routeGrantAuditFields(userID pgtype.UUID) map[string]any {
	return map[string]any{"route_grant_user_id": uuidToString(userID)}
}

// auditThenEnqueue writes the audit row in a transaction, runs enqueue (a
// request store that lives outside Postgres) and commits last. A failed audit
// insert returns an auditWriteError and enqueue never runs; a failed enqueue
// rolls the row back. ponytail: a commit failure after a successful enqueue
// leaves one unaudited request; the store has no undo, add one if that matters.
func (h *Handler) auditThenEnqueue(ctx context.Context, r *http.Request, in auditInput, enqueue func() error) error {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := h.writeAudit(ctx, h.Queries.WithTx(tx), r, in); err != nil {
		return auditWriteError{err}
	}
	if err := enqueue(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// wakeupDefinitionAuditFields snapshots one stored definition; revision 0 means
// absent. config stays out of Allow, so a change stores only the marker.
func wakeupDefinitionAuditFields(ruleKey string, revision int64, config []byte) map[string]any {
	if revision == 0 {
		return map[string]any{}
	}
	return map[string]any{"rule_key": ruleKey, "revision": revision, "config": string(config)}
}
