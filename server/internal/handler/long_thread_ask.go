package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Long-thread route (T5b, CHE-6432): an agent that tags a member deep in a long
// thread asks in a new short thread instead. The route moves or warns; it never
// refuses, so it is not a gate and writes no block row (the recovery and repair
// workers read block rows only).

const (
	longThreadModeKey      = "long_thread_member_ask"
	longThreadMinKey       = "long_thread_member_ask_min"
	longThreadMinDefault   = 10
	longThreadMinLow       = 3
	longThreadMinHigh      = 100
	longThreadRecentWindow = 3
	longThreadAskEventKind = "long_thread_member_ask"
	longThreadModeOff      = "off"
	longThreadModeWarn     = "warn"
	longThreadModeMove     = "move"
	longThreadResolveHint  = "long_thread_member_ask: not moved because the comment resolves or supersedes; use --notify-only for an FYI or --from-thread for an ask"
	longThreadPlacementWhy = "long_thread_member_ask"
)

// CommentPlacement tells the author where a comment landed when the server
// routed it away from the thread it named.
type CommentPlacement struct {
	Moved        bool   `json:"moved"`
	SourceRootID string `json:"source_root_id"`
	Reason       string `json:"reason"`
}

type longThreadConfig struct {
	Mode string
	Min  int
}

// longThreadConfigFromSettings reads the two settings. A missing key is the
// default (off, 10). A stored value the validator would have refused reads as
// off: a comment must never fail because of a setting.
func longThreadConfigFromSettings(raw []byte) longThreadConfig {
	off := longThreadConfig{Mode: longThreadModeOff, Min: longThreadMinDefault}
	if len(raw) == 0 {
		return off
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return off
	}
	cfg := off
	if v, ok := settings[longThreadModeKey]; ok {
		mode, _ := v.(string)
		if mode != longThreadModeOff && mode != longThreadModeWarn && mode != longThreadModeMove {
			return off
		}
		cfg.Mode = mode
	}
	if v, ok := settings[longThreadMinKey]; ok {
		min, valid := settingInt(v)
		if !valid || min < longThreadMinLow || min > longThreadMinHigh {
			return off
		}
		cfg.Min = min
	}
	return cfg
}

// settingInt reads a JSON number that holds a whole value.
func settingInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) || n < math.MinInt32 || n > math.MaxInt32 {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// validateLongThreadSettings checks the two long-thread keys of a workspace
// settings object. An absent key is valid; null is not.
func validateLongThreadSettings(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if v, present := config[longThreadModeKey]; present {
		mode, _ := v.(string)
		if mode != longThreadModeOff && mode != longThreadModeWarn && mode != longThreadModeMove {
			return fmt.Errorf("%s must be off, warn or move", longThreadModeKey)
		}
	}
	if v, present := config[longThreadMinKey]; present {
		if min, valid := settingInt(v); !valid || min < longThreadMinLow || min > longThreadMinHigh {
			return fmt.Errorf("%s must be a whole number from %d to %d", longThreadMinKey, longThreadMinLow, longThreadMinHigh)
		}
	}
	return nil
}

// longThreadVerdict is what the route decided for one comment.
type longThreadVerdict struct {
	// Move: post the comment as a new root linked to SourceRootID.
	Move bool
	// Warn: post in place and write a warn row.
	Warn bool
	// Count is the live comment count of the thread, root included.
	Count        int64
	SourceRootID pgtype.UUID
	// Hint is the text the response carries once the comment exists. Empty for
	// a move, whose hint needs the new comment id.
	Hint string
}

// longThreadRouteInput holds the facts of conditions 1 to 4 of T5b, read from
// the request without touching the database.
type longThreadRouteInput struct {
	Issue         db.Issue
	AuthorType    string
	Content       string
	NotifyOnly    bool
	DecisionKind  bool
	HandoffParent bool
	// ResolveOrSupersede is condition 4: a resolve intent or supersedes targets.
	ResolveOrSupersede bool
	Parent             *db.Comment
	FromThread         bool
}

// longThreadRoute decides whether a comment moves, warns or stays. It reads the
// database only once conditions 1 to 3 hold and the workspace has the route on.
// A failure reads as "stay": the route never blocks a comment.
func (h *Handler) longThreadRoute(ctx context.Context, in longThreadRouteInput) longThreadVerdict {
	if in.AuthorType != "agent" || in.Parent == nil || in.FromThread || in.NotifyOnly || in.DecisionKind || in.HandoffParent {
		return longThreadVerdict{}
	}
	members := askguard.MentionedMembers(in.Content)
	if len(members) == 0 {
		return longThreadVerdict{}
	}
	cfg, root, count, ok := h.longThreadThreadFacts(ctx, in.Issue, in.Parent.ID, members)
	if !ok || cfg.Mode == longThreadModeOff {
		return longThreadVerdict{}
	}
	verdict := longThreadVerdict{Count: count, SourceRootID: root}
	switch {
	case cfg.Mode == longThreadModeWarn:
		verdict.Warn = true
		verdict.Hint = longThreadWarnHint(in.Issue, root, count)
	case in.ResolveOrSupersede:
		verdict.Warn = true
		verdict.Hint = longThreadResolveHint
	default:
		verdict.Move = true
	}
	return verdict
}

// longThreadThreadFacts reads the settings and, when the route is on, conditions
// 5 and 6: the thread is at least Min comments long and none of members wrote
// one of its last longThreadRecentWindow comments. ok is false when the route
// does not apply or a read failed.
func (h *Handler) longThreadThreadFacts(ctx context.Context, issue db.Issue, target pgtype.UUID, members []string) (longThreadConfig, pgtype.UUID, int64, bool) {
	workspace, err := h.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		slog.Info("long_thread_member_ask: workspace read failed", "error", err, "issue_id", uuidToString(issue.ID))
		return longThreadConfig{}, pgtype.UUID{}, 0, false
	}
	cfg := longThreadConfigFromSettings(workspace.Settings)
	if cfg.Mode == longThreadModeOff {
		return cfg, pgtype.UUID{}, 0, true
	}
	count, err := h.Queries.CountThreadLiveComments(ctx, db.CountThreadLiveCommentsParams{TargetID: target, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil || count < int64(cfg.Min) {
		if err != nil {
			slog.Info("long_thread_member_ask: thread count failed", "error", err, "issue_id", uuidToString(issue.ID))
		}
		return cfg, pgtype.UUID{}, 0, false
	}
	recent, err := h.Queries.ListThreadLastAuthors(ctx, db.ListThreadLastAuthorsParams{TargetID: target, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, RowLimit: longThreadRecentWindow})
	if err != nil {
		slog.Info("long_thread_member_ask: recent authors failed", "error", err, "issue_id", uuidToString(issue.ID))
		return cfg, pgtype.UUID{}, 0, false
	}
	mentioned := make(map[string]bool, len(members))
	for _, id := range members {
		mentioned[id] = true
	}
	for _, author := range recent {
		if author.AuthorType == "member" && mentioned[uuidToString(author.AuthorID)] {
			return cfg, pgtype.UUID{}, 0, false
		}
	}
	root, err := h.Queries.GetThreadRoot(ctx, db.GetThreadRootParams{CommentID: target, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		slog.Info("long_thread_member_ask: thread root failed", "error", err, "issue_id", uuidToString(issue.ID))
		return cfg, pgtype.UUID{}, 0, false
	}
	return cfg, root.ID, count, true
}

func longThreadWarnHint(issue db.Issue, root pgtype.UUID, count int64) string {
	return fmt.Sprintf("long_thread_member_ask: thread %s has %d comments. Next time: multica issue comment add %s --from-thread %s --content-file <file>",
		uuidToString(root), count, uuidToString(issue.ID), uuidToString(root))
}

func longThreadMovedHint(commentID pgtype.UUID, v longThreadVerdict) string {
	return fmt.Sprintf("Moved: this ask is new thread %s (thread %s has %d comments). The source thread got a link.",
		uuidToString(commentID), uuidToString(v.SourceRootID), v.Count)
}

// longThreadOp builds the guard event for one operation. payloadKey is
// "<thread root id>:<content>" for a create and "<comment id>:<new content>"
// for an edit. ok is false when the author has no task: task_id is NOT NULL, so
// the call writes no row and logs one INFO line.
func longThreadOp(issue db.Issue, task *db.AgentTaskQueue, payloadKey string) (askguard.Op, bool) {
	if task == nil {
		slog.Info("long_thread_member_ask: no task id, no ask_guard_event row written", "issue_id", uuidToString(issue.ID))
		return askguard.Op{}, false
	}
	return askguard.Op{IssueID: issue.ID, TaskID: task.ID, Kind: longThreadAskEventKind, Payload: []byte(payloadKey)}, true
}

// recordLongThreadWarn writes the warn row of a comment that stays in place. A
// failed write is logged at INFO: the route never fails a comment.
func (h *Handler) recordLongThreadWarn(ctx context.Context, issue db.Issue, task *db.AgentTaskQueue, payloadKey string) {
	op, ok := longThreadOp(issue, task, payloadKey)
	if !ok {
		return
	}
	if err := (askguard.Guard{Queries: h.Queries, Metrics: h.Metrics}).RecordWarn(ctx, issue.WorkspaceID, op); err != nil {
		slog.Info("long_thread_member_ask: warn row failed", "error", err, "issue_id", uuidToString(issue.ID))
	}
}

// longThreadEditHint is the edit rule: an edit never moves a comment. An edit
// by its agent author that adds a member mention to a comment in a long thread,
// where none of the added members wrote a recent comment, writes a warn row and
// returns the from-thread hint, under warn and move. It returns "" when the
// rule does not apply.
func (h *Handler) longThreadEditHint(ctx context.Context, issue db.Issue, existing db.Comment, task *db.AgentTaskQueue, newContent string) string {
	if existing.AuthorType != "agent" || (existing.NotifyOnly.Valid && existing.NotifyOnly.Bool) {
		return ""
	}
	old := make(map[string]bool)
	for _, id := range askguard.MentionedMembers(existing.Content) {
		old[id] = true
	}
	var added []string
	for _, id := range askguard.MentionedMembers(newContent) {
		if !old[id] {
			added = append(added, id)
		}
	}
	if len(added) == 0 {
		return ""
	}
	cfg, root, count, ok := h.longThreadThreadFacts(ctx, issue, existing.ID, added)
	if !ok || cfg.Mode == longThreadModeOff {
		return ""
	}
	h.recordLongThreadWarn(ctx, issue, task, uuidToString(existing.ID)+":"+newContent)
	return longThreadWarnHint(issue, root, count)
}
