package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type blockedDispatchInputs struct {
	Reason   string
	PRState  string
	Comments string
}

func blockedDispatchFingerprint(inputs blockedDispatchInputs) string {
	b, _ := json.Marshal(inputs)
	return blockedDispatchFingerprintRaw(string(b))
}

func blockedDispatchFingerprintRaw(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func blockedDispatchUnchanged(actorType, previous, current string) bool {
	return actorType != "member" && previous != "" && previous == current
}

func (s *TaskService) stampBlockedDispatchSnapshot(ctx context.Context, q *db.Queries, issue db.Issue, task db.AgentTaskQueue) {
	if issue.Status != "blocked" {
		return
	}
	inputs, err := q.GetBlockedDispatchFingerprint(ctx, db.GetBlockedDispatchFingerprintParams{IssueID: issue.ID, TofuID: task.AgentID})
	if err != nil {
		slog.Warn("blocked dispatch snapshot unavailable; future dispatch will fail open", "issue_id", util.UUIDToString(issue.ID), "error", err)
		return
	}
	fingerprint := blockedDispatchFingerprintRaw(inputs)
	if err := q.StampBlockedDispatchFingerprint(ctx, db.StampBlockedDispatchFingerprintParams{TaskID: task.ID, Fingerprint: fingerprint}); err != nil {
		slog.Warn("blocked dispatch snapshot could not be stored; future dispatch will fail open", "issue_id", util.UUIDToString(issue.ID), "task_id", util.UUIDToString(task.ID), "error", err)
	}
}

func blockedDispatchMode(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) string {
	workspace, err := q.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "off"
	}
	var settings struct {
		Dedup struct {
			Mode string `json:"mode"`
		} `json:"blocked_dispatch_dedup"`
	}
	if err := json.Unmarshal(workspace.Settings, &settings); err != nil {
		return "off"
	}
	mode := settings.Dedup.Mode
	if mode == "" {
		return "block"
	}
	if mode != "off" && mode != "warn" && mode != "block" {
		return "off"
	}
	return mode
}

// SkipBlockedDispatch reports a duplicate blocked-step dispatch only when the
// latest completed run captured the exact current inputs. Member transitions
// are explicit requests and always get their run.
func (s *TaskService) SkipBlockedDispatch(ctx context.Context, issue db.Issue, tofuID pgtype.UUID, actorType string) (pgtype.UUID, bool, error) {
	if actorType == "member" || issue.Status != "blocked" || !tofuID.Valid {
		return pgtype.UUID{}, false, nil
	}
	mode := blockedDispatchMode(ctx, s.Queries, issue.WorkspaceID)
	if mode == "off" {
		return pgtype.UUID{}, false, nil
	}
	last, err := s.Queries.GetLatestCompletedBlockedDispatchTask(ctx, db.GetLatestCompletedBlockedDispatchTaskParams{IssueID: issue.ID, AgentID: tofuID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, false, nil
		}
		return pgtype.UUID{}, false, err
	}
	previous := last.Fingerprint
	if previous == "" {
		return pgtype.UUID{}, false, nil
	}
	inputs, err := s.Queries.GetBlockedDispatchFingerprint(ctx, db.GetBlockedDispatchFingerprintParams{IssueID: issue.ID, TofuID: tofuID})
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	unchanged := blockedDispatchUnchanged(actorType, previous, blockedDispatchFingerprintRaw(inputs))
	if unchanged && mode == "warn" {
		slog.Info("blocked dispatch dedup would skip run", "issue_id", util.UUIDToString(issue.ID), "task_id", util.UUIDToString(last.ID))
	}
	return last.ID, unchanged && mode == "block", nil
}

func (s *TaskService) RecordBlockedDispatchSkipped(ctx context.Context, issue db.Issue, lastTask pgtype.UUID, actorType, actorID string) {
	targetID := util.UUIDToString(issue.AssigneeID)
	details, _ := json.Marshal(map[string]any{
		"rule": "blocked_status_dispatch", "reason": "unchanged inputs", "last_task_id": util.UUIDToString(lastTask),
		"events": []string{"issue.status_changed"}, "actor_type": actorType, "actor_id": actorID,
		"target_type": issue.AssigneeType.String, "target_id": targetID,
	})
	actorUUID, _ := util.ParseUUID(actorID)
	row, err := s.Queries.CreateActivity(ctx, db.CreateActivityParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		ActorType: pgtype.Text{String: actorType, Valid: actorType != ""}, ActorID: actorUUID, Action: "wakeup_skipped", Details: details,
	})
	if err != nil {
		slog.Warn("blocked dispatch skip timeline entry failed", "issue_id", util.UUIDToString(issue.ID), "error", err)
		return
	}
	if s.Bus != nil {
		s.Bus.Publish(events.Event{Type: protocol.EventActivityCreated, WorkspaceID: util.UUIDToString(issue.WorkspaceID), ActorType: "system", Payload: map[string]any{
			"issue_id": util.UUIDToString(issue.ID),
			"entry":    map[string]any{"type": "activity", "id": util.UUIDToString(row.ID), "actor_type": actorType, "action": row.Action, "details": json.RawMessage(details), "created_at": row.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")},
		}})
	}
}
