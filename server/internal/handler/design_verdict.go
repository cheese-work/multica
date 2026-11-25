package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/designguard"
	"github.com/multica-ai/multica/server/internal/doopread"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Typed design review verdict record (CHE-7064 D1.4). A reviewer agent writes
// PASS or CHANGES for one doop attestation from its own run on the issue. The
// record is one activity_log row; the card guard reads the newest one per issue
// and attestation (GetNewestDesignVerdict). There is no edit or delete route.

const (
	activityDesignVerdict = "design_review_verdict"

	codeDesignVerdictRunRequired      = "design_verdict_run_required"
	codeDesignVerdictBadBody          = "design_verdict_bad_body"
	codeDesignVerdictFindingsRequired = "design_verdict_findings_required"

	maxDesignVerdictBody     = 64 << 10
	maxDesignVerdictFindings = 50
	maxDesignFindingSummary  = 2000
)

var designAttestationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type designVerdictBody struct {
	AttestationID string          `json:"attestation_id"`
	Verdict       string          `json:"verdict"`
	Findings      []ReviewFinding `json:"findings"`
}

// RecordDesignVerdict is POST /api/issues/{id}/design-verdicts, task token only.
// 201 on a record; 403 without a run on this issue; 400 for a bad body; 404 for
// an unknown attestation; 409 for a failed C2 to C5 or A1 to A5 (PASS only);
// 503 when doop is unavailable or the guard is misconfigured.
func (h *Handler) RecordDesignVerdict(w http.ResponseWriter, r *http.Request) {
	denied := func() {
		writeErrorCode(w, http.StatusForbidden, codeDesignVerdictRunRequired, "a design verdict must come from the agent's run on this issue (a task token whose task is dispatched or running on this issue)")
	}
	if r.Header.Get("X-Actor-Source") != "task_token" {
		denied()
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	ctx := r.Context()
	taskID, taskErr := util.ParseUUID(r.Header.Get("X-Task-ID"))
	agentID, agentErr := util.ParseUUID(r.Header.Get("X-Agent-ID"))
	if taskErr != nil || agentErr != nil {
		denied()
		return
	}
	task, err := h.Queries.GetAgentTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (task.AgentID != agentID || task.IssueID != issue.ID || task.Status != "dispatched" && task.Status != "running") {
		denied()
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the task")
		return
	}

	body, refusal := decodeDesignVerdictBody(w, r)
	if refusal != nil {
		writeErrorCode(w, http.StatusBadRequest, refusal.Code, refusal.Message)
		return
	}

	workspace, err := h.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the workspace")
		return
	}
	settings, err := designguard.ParseSettings(workspace.Settings)
	if err != nil || settings.Mode == designguard.ModeOff {
		msg := "design_card_guard is off"
		if err != nil {
			msg = err.Error()
		}
		writeErrorCode(w, http.StatusServiceUnavailable, designguard.CodeGuardMisconfigured, msg)
		return
	}

	view, err := h.DoopReader.Attestation(ctx, body.AttestationID)
	switch {
	case errors.Is(err, doopread.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, designguard.CodeAttestationNotFound, "doop has no attestation with this id")
		return
	case err != nil:
		writeErrorCode(w, http.StatusServiceUnavailable, designguard.CodeDoopUnavailable, "doop is unavailable, so the attestation cannot be read")
		return
	}

	ids := []string{uuidToString(taskID)}
	for _, a := range view.Authors {
		if a.Kind == "agent" && a.TaskID != nil {
			ids = append(ids, *a.TaskID)
		}
	}
	rows, err := h.designTaskRows(ctx, ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the tasks")
		return
	}
	reviewer := designguard.Reviewer{AgentID: uuidToString(agentID), TaskID: uuidToString(taskID)}

	if body.Verdict == "PASS" {
		attestation := designguard.Attestation{CanvasID: view.CanvasID, PageID: view.PageID, PageIssueID: view.PageIssueID, Complete: view.Complete, Current: view.Current}
		for _, f := range view.Frames {
			attestation.Frames = append(attestation.Frames, f.FrameID)
		}
		if refusal := designguard.CheckVerdictTarget(settings.DoopOrigin, designURLOf(issue, settings.DesignURLPropertyID), uuidToString(issue.ID), attestation); refusal != nil {
			writeErrorCode(w, http.StatusConflict, refusal.Code, refusal.Message)
			return
		}
		code, err := designguard.CheckIndependence(ctx, independenceView(view), reviewer, "", designguard.Deps{
			Tasks: func(context.Context, []string) ([]designguard.Task, error) { return rows, nil },
			Lab:   voteLab,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check reviewer independence")
			return
		}
		if code != "" {
			writeErrorCode(w, http.StatusConflict, code, "the reviewer is not independent of the page's authors ("+code+")")
			return
		}
	}

	byID := make(map[string]designguard.Task, len(rows))
	for _, t := range rows {
		byID[t.ID] = t
	}
	authorAgents, authorLabs := []string{}, []string{}
	for _, a := range view.Authors {
		if a.Kind != "agent" || a.AgentID == nil {
			continue
		}
		if !slices.Contains(authorAgents, *a.AgentID) {
			authorAgents = append(authorAgents, *a.AgentID)
		}
		if a.TaskID == nil {
			continue
		}
		if t, found := byID[*a.TaskID]; found {
			if lab := voteLab(t.Provider, t.Model); lab != "" && !slices.Contains(authorLabs, lab) {
				authorLabs = append(authorLabs, lab)
			}
		}
	}
	slices.Sort(authorAgents)
	slices.Sort(authorLabs)
	frames := make([]string, 0, len(view.Frames))
	for _, f := range view.Frames {
		frames = append(frames, f.FrameID)
	}
	if body.Findings == nil {
		body.Findings = []ReviewFinding{}
	}
	self := byID[reviewer.TaskID]
	details, _ := json.Marshal(map[string]any{
		"attestation_id": body.AttestationID, "verdict": body.Verdict, "findings": body.Findings,
		"canvas_id": view.CanvasID, "page_id": view.PageID, "ruleset": view.Ruleset, "frames": frames,
		"task_id": reviewer.TaskID, "runtime_provider": self.Provider, "claimed_model": self.Model,
		"author_agents": authorAgents, "author_labs": authorLabs,
	})
	row, err := h.Queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		ActorType: pgtype.Text{String: "agent", Valid: true}, ActorID: agentID,
		Action: activityDesignVerdict, Details: details,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record the verdict")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": uuidToString(row.ID), "attestation_id": body.AttestationID, "verdict": body.Verdict})
}

// decodeDesignVerdictBody reads and checks the body: strict fields, a PASS or
// CHANGES verdict, valid findings, and for CHANGES at least one P0 or P1.
func decodeDesignVerdictBody(w http.ResponseWriter, r *http.Request) (designVerdictBody, *designguard.Refusal) {
	var body designVerdictBody
	bad := func(msg string) (designVerdictBody, *designguard.Refusal) {
		return body, &designguard.Refusal{Code: codeDesignVerdictBadBody, Message: msg}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDesignVerdictBody))
	if err != nil {
		return bad("the body could not be read")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return bad("the body must be JSON with attestation_id, verdict and findings")
	}
	if _, err := dec.Token(); err != io.EOF {
		return bad("the body must hold one JSON value")
	}
	switch {
	case !designAttestationIDPattern.MatchString(body.AttestationID):
		return bad("attestation_id must match ^[A-Za-z0-9_-]{1,64}$")
	case body.Verdict != "PASS" && body.Verdict != "CHANGES":
		return bad("verdict must be PASS or CHANGES")
	case len(body.Findings) > maxDesignVerdictFindings:
		return bad("too many findings")
	}
	for _, f := range body.Findings {
		summary := strings.TrimSpace(f.Summary)
		if !slices.Contains([]string{"P0", "P1", "P2", "P3"}, f.Severity) || summary == "" || len(summary) > maxDesignFindingSummary {
			return bad("each finding needs a severity P0 to P3 and a summary of at most 2000 bytes")
		}
	}
	if body.Verdict == "CHANGES" && !blocking(body.Findings) {
		return body, &designguard.Refusal{Code: codeDesignVerdictFindingsRequired, Message: errFindingsRequired.Error()}
	}
	return body, nil
}

// designURLOf is the issue's Design url property value, or "" when unset.
func designURLOf(issue db.Issue, propertyID string) string {
	var props map[string]any
	if json.Unmarshal(issue.Properties, &props) != nil {
		return ""
	}
	value, _ := props[propertyID].(string)
	return value
}

func independenceView(view doopread.View) designguard.View {
	out := designguard.View{ProvenanceComplete: view.ProvenanceComplete}
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	for _, a := range view.Authors {
		out.Authors = append(out.Authors, designguard.Author{Kind: a.Kind, AgentID: deref(a.AgentID), TaskID: deref(a.TaskID)})
	}
	return out
}

// designTaskRows reads the Multica task rows behind ids. An id that is not a
// UUID, or that Multica does not know, is absent from the result.
func (h *Handler) designTaskRows(ctx context.Context, ids []string) ([]designguard.Task, error) {
	var uuids []pgtype.UUID
	for _, id := range ids {
		if u, err := util.ParseUUID(id); err == nil {
			uuids = append(uuids, u)
		}
	}
	rows, err := h.Queries.ListDesignAuthorTasks(ctx, uuids)
	if err != nil {
		return nil, err
	}
	out := make([]designguard.Task, 0, len(rows))
	for _, t := range rows {
		out = append(out, designguard.Task{ID: uuidToString(t.ID), AgentID: uuidToString(t.AgentID), Provider: t.Provider, Model: t.ClaimedModel})
	}
	return out, nil
}
