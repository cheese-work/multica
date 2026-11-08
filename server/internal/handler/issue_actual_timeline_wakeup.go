package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

// Wakeup events in the timeline response (CHE-4312 S2b). Each event is one
// durable wakeup_triggered activity row that started a run, classified from
// its own rule snapshot (details.wakeup) so it survives edits and deletion of
// the rule. The response carries identifiers and classes only: no instruction
// text, comment bodies or filter values.

const (
	wakeupClassSelf              = "self"
	wakeupClassCross             = "cross"
	wakeupClassAggregateChildren = "aggregate_children"
	wakeupClassExternal          = "external"
	wakeupClassUnknownSource     = "unknown_source"

	timelineMaxWakeupEvents = 5000
)

type timelineWakeupEvent struct {
	ID            string    `json:"id"`
	At            time.Time `json:"at"`
	TargetIssueID string    `json:"target_issue_id"`
	RuleID        string    `json:"rule_id"`
	RuleKind      string    `json:"rule_kind"`
	ConditionType string    `json:"condition_type"`
	SourceIssueID *string   `json:"source_issue_id"`
	Class         string    `json:"class"`
	Stage         *int32    `json:"stage,omitempty"`
	RunID         *string   `json:"run_id"`
}

type wakeupRowDetails struct {
	TaskID string `json:"task_id"`
	Wakeup *struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		AgentID   string `json:"agent_id"`
		Condition *struct {
			Type  string `json:"type"`
			Stage *int32 `json:"stage"`
		} `json:"condition"`
	} `json:"wakeup"`
	Observed *struct {
		IssueID string `json:"issue_id"`
	} `json:"observed"`
}

// classifyWakeup maps a snapshot to an event class and its source issue. A row
// whose condition has no recorded observation is a marker only: the source
// is not guessed from the rule's current or snapshot condition.
func classifyWakeup(d wakeupRowDetails) (class, conditionType string, source *string, stage *int32) {
	if d.Wakeup == nil {
		return wakeupClassUnknownSource, "", nil, nil
	}
	if d.Wakeup.Condition == nil {
		return wakeupClassSelf, "", nil, nil // time rules and event rules carry no condition
	}
	conditionType = d.Wakeup.Condition.Type
	switch conditionType {
	case "other_issue":
		if d.Observed == nil || d.Observed.IssueID == "" {
			return wakeupClassUnknownSource, conditionType, nil, nil
		}
		return wakeupClassCross, conditionType, &d.Observed.IssueID, nil
	case "children_done":
		return wakeupClassAggregateChildren, conditionType, nil, d.Wakeup.Condition.Stage
	case "pull_request", "github_run":
		return wakeupClassExternal, conditionType, nil, nil
	default:
		return wakeupClassUnknownSource, conditionType, nil, nil
	}
}

// loadTimelineWakeupEvents returns the run-starting wakeup events targeting
// the page's issues inside [from, to) and at or before asOf. Events whose
// agent the viewer cannot see, or whose source issue is gone or outside the
// workspace, are omitted.
func (h *Handler) loadTimelineWakeupEvents(w http.ResponseWriter, r *http.Request, workspaceID string, issueIDs []string, from, to, asOf time.Time) ([]timelineWakeupEvent, bool) {
	events := make([]timelineWakeupEvent, 0)
	if len(issueIDs) == 0 {
		return events, true
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return nil, false
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	allowed, ok := h.accessibleAgentIDs(r.Context(), workspaceID, actorType, actorID, member.Role)
	if !ok {
		writeError(w, http.StatusInternalServerError, "failed to resolve agent access")
		return nil, false
	}
	rows, err := h.DB.Query(r.Context(), `
SELECT id::text, issue_id::text, created_at, details
FROM activity_log
WHERE workspace_id = $1 AND issue_id = ANY($2::uuid[]) AND action = 'wakeup_triggered'
  AND created_at >= $3 AND created_at < $4 AND created_at <= $5
  AND COALESCE(details->>'task_id', '') <> ''
ORDER BY created_at, id`, workspaceID, issueIDs, from, to, asOf)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load wakeup events")
		return nil, false
	}
	defer rows.Close()
	sources := map[string]struct{}{}
	for rows.Next() {
		var (
			e   timelineWakeupEvent
			raw []byte
			d   wakeupRowDetails
		)
		if err := rows.Scan(&e.ID, &e.TargetIssueID, &e.At, &raw); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load wakeup events")
			return nil, false
		}
		if json.Unmarshal(raw, &d) != nil {
			d = wakeupRowDetails{} // unreadable details stay a marker
		}
		if d.Wakeup != nil && d.Wakeup.AgentID != "" {
			if _, ok := allowed[d.Wakeup.AgentID]; !ok {
				continue
			}
		}
		e.Class, e.ConditionType, e.SourceIssueID, e.Stage = classifyWakeup(d)
		if d.Wakeup != nil {
			e.RuleID, e.RuleKind = d.Wakeup.ID, d.Wakeup.Kind
		}
		if d.TaskID != "" {
			e.RunID = &d.TaskID
		}
		if e.SourceIssueID != nil {
			if _, err := util.ParseUUID(*e.SourceIssueID); err != nil {
				e.Class, e.SourceIssueID = wakeupClassUnknownSource, nil
			} else {
				sources[*e.SourceIssueID] = struct{}{}
			}
		}
		events = append(events, e)
	}
	if rows.Err() != nil {
		writeError(w, http.StatusInternalServerError, "failed to load wakeup events")
		return nil, false
	}
	rows.Close()
	live, err := h.workspaceIssueSet(r.Context(), workspaceID, sources)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load wakeup events")
		return nil, false
	}
	kept := events[:0]
	for _, e := range events {
		if e.SourceIssueID != nil {
			if _, ok := live[*e.SourceIssueID]; !ok {
				continue
			}
		}
		kept = append(kept, e)
	}
	// The cap counts events the viewer can see, so hidden rows never use it up.
	if len(kept) > timelineMaxWakeupEvents {
		writeError(w, http.StatusUnprocessableEntity, "too many wakeup events for this window; narrow the window")
		return nil, false
	}
	return kept, true
}

// workspaceIssueSet returns the subset of ids that are issues of the workspace.
func (h *Handler) workspaceIssueSet(ctx context.Context, workspaceID string, ids map[string]struct{}) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	rows, err := h.DB.Query(ctx, `SELECT id::text FROM issue WHERE workspace_id = $1 AND id = ANY($2::uuid[])`, workspaceID, list)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}
