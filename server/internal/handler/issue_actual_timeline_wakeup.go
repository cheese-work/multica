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

// wakeupBatchSize is the keyset batch size; a var so a test can use small batches.
var wakeupBatchSize = timelineMaxWakeupEvents + 1

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
	allowedIDs := make([]string, 0, len(allowed))
	for id := range allowed {
		allowedIDs = append(allowedIDs, id)
	}
	// Rows are read in keyset batches and filtered batch by batch, so one
	// request holds at most the visible events plus one batch, and stops
	// reading as soon as the visible events pass the cap.
	cursorAt, cursorID := from, "00000000-0000-0000-0000-000000000000"
	for {
		batch, last, err := h.loadWakeupBatch(r.Context(), workspaceID, issueIDs, allowedIDs, allowed, from, to, asOf, cursorAt, cursorID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load wakeup events")
			return nil, false
		}
		events = append(events, batch.kept...)
		// The cap counts events the viewer can see, so hidden rows never use it up.
		if len(events) > timelineMaxWakeupEvents {
			writeError(w, http.StatusUnprocessableEntity, "too many wakeup events for this window; narrow the window")
			return nil, false
		}
		if batch.rows < wakeupBatchSize {
			return events, true
		}
		cursorAt, cursorID = last.At, last.ID
	}
}

type wakeupBatch struct {
	kept []timelineWakeupEvent
	rows int // rows read, before filtering
}

// loadWakeupBatch reads the next batch of rows after the cursor and returns the
// events the viewer can see. The returned last event is the batch's last row
// (kept or not), which is the next cursor.
func (h *Handler) loadWakeupBatch(ctx context.Context, workspaceID string, issueIDs, allowedIDs []string, allowed map[string]struct{}, from, to, asOf, cursorAt time.Time, cursorID string) (wakeupBatch, timelineWakeupEvent, error) {
	var (
		out  wakeupBatch
		last timelineWakeupEvent
	)
	rows, err := h.DB.Query(ctx, `
SELECT id::text, issue_id::text, created_at, details
FROM activity_log
WHERE workspace_id = $1 AND issue_id = ANY($2::uuid[]) AND action = 'wakeup_triggered'
  AND created_at >= $3 AND created_at < $4 AND created_at <= $5
  AND (created_at, id) > ($6, $7::uuid)
  AND COALESCE(details->>'task_id', '') <> ''
  AND (COALESCE(details->'wakeup'->>'agent_id', '') = '' OR details->'wakeup'->>'agent_id' = ANY($8::text[]))
ORDER BY created_at, id
LIMIT $9`, workspaceID, issueIDs, from, to, asOf, cursorAt, cursorID, allowedIDs, wakeupBatchSize)
	if err != nil {
		return out, last, err
	}
	defer rows.Close()
	var events []timelineWakeupEvent
	sources := map[string]struct{}{}
	for rows.Next() {
		var (
			e   timelineWakeupEvent
			raw []byte
			d   wakeupRowDetails
		)
		if err := rows.Scan(&e.ID, &e.TargetIssueID, &e.At, &raw); err != nil {
			return out, last, err
		}
		out.rows++
		last = e
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
	if err := rows.Err(); err != nil {
		return out, last, err
	}
	rows.Close()
	live, err := h.workspaceIssueSet(ctx, workspaceID, sources)
	if err != nil {
		return out, last, err
	}
	for _, e := range events {
		if e.SourceIssueID != nil {
			if _, ok := live[*e.SourceIssueID]; !ok {
				continue
			}
		}
		out.kept = append(out.kept, e)
	}
	return out, last, nil
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
