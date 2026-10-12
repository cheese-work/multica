package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	governanceCaseAuditDefaultPageSize = 25
	governanceCaseAuditMaxPageSize     = 50
)

type governanceCaseAuditItem struct {
	ID                 string  `json:"id"`
	SubjectType        string  `json:"subject_type"`
	SubjectID          string  `json:"subject_id"`
	SubjectRevision    int64   `json:"subject_revision"`
	RuleID             string  `json:"rule_id"`
	Generation         int32   `json:"generation"`
	State              string  `json:"state"`
	StateRevision      int64   `json:"state_revision"`
	RuleRevision       string  `json:"rule_revision"`
	ActivationRevision string  `json:"activation_revision"`
	ConfigRevision     string  `json:"config_revision"`
	EvidenceEpoch      int32   `json:"evidence_epoch"`
	RefreshCount       int32   `json:"refresh_count"`
	AbsoluteDeadline   *string `json:"absolute_deadline,omitempty"`
	CreatedAt          string  `json:"created_at"`
	UpdatedAt          string  `json:"updated_at"`
}

type governanceCaseAuditListResponse struct {
	Cases      []governanceCaseAuditItem  `json:"cases"`
	NextCursor *governanceCaseAuditCursor `json:"next_cursor,omitempty"`
}

type governanceCaseAuditCursor struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

type governanceCaseAuditResponse struct {
	Case                  governanceCaseAuditItem              `json:"case"`
	Transitions           []governanceCaseAuditTransition      `json:"transitions"`
	TransitionsTruncated  bool                                 `json:"transitions_truncated"`
	TransitionsNextCursor *governanceCaseAuditCursor           `json:"transitions_next_cursor,omitempty"`
	Attempts              []governanceCaseAuditAttempt         `json:"attempts"`
	AttemptsTruncated     bool                                 `json:"attempts_truncated"`
	AttemptsNextCursor    *governanceCaseAuditCursor           `json:"attempts_next_cursor,omitempty"`
	Evaluations           []governanceCaseAuditEvaluation      `json:"evaluations"`
	EvaluationsTruncated  bool                                 `json:"evaluations_truncated"`
	EvaluationsNextCursor *governanceCaseAuditEvaluationCursor `json:"evaluations_next_cursor,omitempty"`
}

type governanceCaseAuditEvaluationCursor struct {
	CapturedAt string `json:"captured_at"`
	ID         string `json:"id"`
}

type governanceCaseAuditTransition struct {
	ID                     string `json:"id"`
	ResultingStateRevision int64  `json:"resulting_state_revision"`
	ExpectedStateRevision  int64  `json:"expected_state_revision"`
	FromState              string `json:"from_state"`
	ToState                string `json:"to_state"`
	ActorType              string `json:"actor_type"`
	CreatedAt              string `json:"created_at"`
}

type governanceCaseAuditAttempt struct {
	ID           string  `json:"id"`
	Ordinal      int32   `json:"ordinal"`
	Kind         string  `json:"kind"`
	ObligationID *string `json:"obligation_id,omitempty"`
	DeadlineAt   *string `json:"deadline_at,omitempty"`
	TerminalAt   *string `json:"terminal_at,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

type governanceCaseAuditEvaluation struct {
	ID                    string  `json:"id"`
	SnapshotSchemaVersion int16   `json:"snapshot_schema_version"`
	RequiredComplete      bool    `json:"required_complete"`
	EstimatedTokens       int32   `json:"estimated_tokens"`
	CapturedAt            string  `json:"captured_at"`
	RedactedAt            *string `json:"redacted_at,omitempty"`
}

func (h *Handler) governanceCaseAuditWorkspace(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return pgtype.UUID{}, false
	}
	if !featureflags.GovernanceCaseAuditEnabled(r.Context(), h.FeatureFlags) {
		writeError(w, http.StatusServiceUnavailable, "governance case audit is currently disabled")
		return pgtype.UUID{}, false
	}
	workspaceID := h.resolveWorkspaceID(r)
	workspaceUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid workspace id")
		return pgtype.UUID{}, false
	}
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return pgtype.UUID{}, false
	}
	return workspaceUUID, true
}

func governanceCaseAuditPageSize(w http.ResponseWriter, r *http.Request) (int32, bool) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return governanceCaseAuditDefaultPageSize, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > governanceCaseAuditMaxPageSize {
		writeError(w, http.StatusBadRequest, "limit must be between 1 and 50")
		return 0, false
	}
	return int32(limit), true
}

func governanceCaseAuditCursorParams(w http.ResponseWriter, r *http.Request, atParam, idParam string) (bool, pgtype.Timestamptz, pgtype.UUID, bool) {
	atValue := strings.TrimSpace(r.URL.Query().Get(atParam))
	idValue := strings.TrimSpace(r.URL.Query().Get(idParam))
	if atValue == "" && idValue == "" {
		return false, pgtype.Timestamptz{}, pgtype.UUID{}, true
	}
	if atValue == "" || idValue == "" {
		writeError(w, http.StatusBadRequest, "both cursor fields are required")
		return false, pgtype.Timestamptz{}, pgtype.UUID{}, false
	}
	cursorAt, err := time.Parse(time.RFC3339Nano, atValue)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid cursor timestamp")
		return false, pgtype.Timestamptz{}, pgtype.UUID{}, false
	}
	cursorID, err := util.ParseUUID(idValue)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid cursor id")
		return false, pgtype.Timestamptz{}, pgtype.UUID{}, false
	}
	return true, pgtype.Timestamptz{Time: cursorAt, Valid: true}, cursorID, true
}

func governanceCaseAuditCursorFor(createdAt pgtype.Timestamptz, id pgtype.UUID) *governanceCaseAuditCursor {
	return &governanceCaseAuditCursor{
		CreatedAt: createdAt.Time.UTC().Format(time.RFC3339Nano),
		ID:        uuidToString(id),
	}
}

func (h *Handler) ListGovernanceCaseAudit(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.governanceCaseAuditWorkspace(w, r)
	if !ok {
		return
	}
	limit, ok := governanceCaseAuditPageSize(w, r)
	if !ok {
		return
	}
	beforeAt := strings.TrimSpace(r.URL.Query().Get("before_created_at"))
	beforeID := strings.TrimSpace(r.URL.Query().Get("before_id"))
	if (beforeAt == "") != (beforeID == "") {
		writeError(w, http.StatusBadRequest, "both cursor fields are required")
		return
	}
	params := db.ListGovernanceCaseAuditPageParams{
		WorkspaceID: workspaceID,
		PageSize:    limit + 1,
		HasCursor:   beforeAt != "",
	}
	if beforeAt != "" {
		createdAt, err := time.Parse(time.RFC3339Nano, beforeAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor timestamp")
			return
		}
		cursorID, err := util.ParseUUID(beforeID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor id")
			return
		}
		params.CursorCreatedAt = pgtype.Timestamptz{Time: createdAt, Valid: true}
		params.CursorID = cursorID
	}
	rows, err := h.Queries.ListGovernanceCaseAuditPage(r.Context(), params)
	if err != nil {
		slog.Error("list governance case audit failed", "workspace_id", uuidToString(workspaceID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list governance cases")
		return
	}
	response := governanceCaseAuditListResponse{Cases: make([]governanceCaseAuditItem, 0, min(len(rows), int(limit)))}
	for _, row := range rows[:min(len(rows), int(limit))] {
		response.Cases = append(response.Cases, governanceCaseAuditItem{
			ID: rowID(row.ID), SubjectType: row.SubjectType, SubjectID: uuidToString(row.SubjectID),
			SubjectRevision: row.SubjectRevision, RuleID: uuidToString(row.RuleID), Generation: row.Generation,
			State: row.State, StateRevision: row.StateRevision, RuleRevision: row.RuleRevision,
			ActivationRevision: row.ActivationRevision, ConfigRevision: row.ConfigRevision,
			EvidenceEpoch: row.EvidenceEpoch, RefreshCount: row.RefreshCount,
			AbsoluteDeadline: timestampToPtr(row.AbsoluteDeadline), CreatedAt: timestampToString(row.CreatedAt),
			UpdatedAt: timestampToString(row.UpdatedAt),
		})
	}
	if len(rows) > int(limit) {
		last := rows[int(limit)-1]
		response.NextCursor = &governanceCaseAuditCursor{
			CreatedAt: last.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
			ID:        uuidToString(last.ID),
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) GetGovernanceCaseAudit(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.governanceCaseAuditWorkspace(w, r)
	if !ok {
		return
	}
	caseID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "caseId"), "case id")
	if !ok {
		return
	}
	response, ok := h.governanceCaseAuditDetail(w, r, workspaceID, caseID)
	if ok {
		writeJSON(w, http.StatusOK, response)
	}
}

func (h *Handler) ExportGovernanceCaseAudit(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.governanceCaseAuditWorkspace(w, r)
	if !ok {
		return
	}
	caseID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "caseId"), "case id")
	if !ok {
		return
	}
	response, ok := h.governanceCaseAuditDetail(w, r, workspaceID, caseID)
	if !ok {
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\"governance-case-"+uuidToString(caseID)+".json\"")
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) governanceCaseAuditDetail(w http.ResponseWriter, r *http.Request, workspaceID, caseID pgtype.UUID) (governanceCaseAuditResponse, bool) {
	limit, ok := governanceCaseAuditPageSize(w, r)
	if !ok {
		return governanceCaseAuditResponse{}, false
	}
	transitionsHasCursor, transitionsCursorAt, transitionsCursorID, ok := governanceCaseAuditCursorParams(
		w, r, "transitions_before_created_at", "transitions_before_id",
	)
	if !ok {
		return governanceCaseAuditResponse{}, false
	}
	attemptsHasCursor, attemptsCursorAt, attemptsCursorID, ok := governanceCaseAuditCursorParams(
		w, r, "attempts_before_created_at", "attempts_before_id",
	)
	if !ok {
		return governanceCaseAuditResponse{}, false
	}
	evaluationsHasCursor, evaluationsCursorAt, evaluationsCursorID, ok := governanceCaseAuditCursorParams(
		w, r, "evaluations_before_captured_at", "evaluations_before_id",
	)
	if !ok {
		return governanceCaseAuditResponse{}, false
	}
	caseRow, err := h.Queries.GetGovernanceCaseAudit(r.Context(), db.GetGovernanceCaseAuditParams{
		WorkspaceID: workspaceID, CaseID: caseID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "governance case not found")
		return governanceCaseAuditResponse{}, false
	}
	if err != nil {
		slog.Error("get governance case audit failed", "workspace_id", uuidToString(workspaceID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load governance case")
		return governanceCaseAuditResponse{}, false
	}
	caseItem := governanceCaseAuditItem{
		ID: uuidToString(caseRow.ID), SubjectType: caseRow.SubjectType, SubjectID: uuidToString(caseRow.SubjectID),
		SubjectRevision: caseRow.SubjectRevision, RuleID: uuidToString(caseRow.RuleID), Generation: caseRow.Generation,
		State: caseRow.State, StateRevision: caseRow.StateRevision, RuleRevision: caseRow.RuleRevision,
		ActivationRevision: caseRow.ActivationRevision, ConfigRevision: caseRow.ConfigRevision,
		EvidenceEpoch: caseRow.EvidenceEpoch, RefreshCount: caseRow.RefreshCount,
		AbsoluteDeadline: timestampToPtr(caseRow.AbsoluteDeadline), CreatedAt: timestampToString(caseRow.CreatedAt),
		UpdatedAt: timestampToString(caseRow.UpdatedAt),
	}
	pageSize := limit + 1
	transitionRows, err := h.Queries.ListGovernanceCaseAuditTransitions(r.Context(), db.ListGovernanceCaseAuditTransitionsParams{
		WorkspaceID: workspaceID, CaseID: caseID, HasCursor: transitionsHasCursor,
		CursorCreatedAt: transitionsCursorAt, CursorID: transitionsCursorID, PageSize: pageSize,
	})
	if err != nil {
		return governanceCaseAuditQueryFailure(w, workspaceID, "list transitions", err)
	}
	attemptRows, err := h.Queries.ListGovernanceCaseAuditAttempts(r.Context(), db.ListGovernanceCaseAuditAttemptsParams{
		WorkspaceID: workspaceID, CaseID: caseID, HasCursor: attemptsHasCursor,
		CursorCreatedAt: attemptsCursorAt, CursorID: attemptsCursorID, PageSize: pageSize,
	})
	if err != nil {
		return governanceCaseAuditQueryFailure(w, workspaceID, "list attempts", err)
	}
	evaluationRows, err := h.Queries.ListGovernanceCaseAuditEvaluations(r.Context(), db.ListGovernanceCaseAuditEvaluationsParams{
		WorkspaceID: workspaceID, CaseID: caseID, HasCursor: evaluationsHasCursor,
		CursorCapturedAt: evaluationsCursorAt, CursorID: evaluationsCursorID, PageSize: pageSize,
	})
	if err != nil {
		return governanceCaseAuditQueryFailure(w, workspaceID, "list evaluations", err)
	}
	response := governanceCaseAuditResponse{
		Case:                 caseItem,
		Transitions:          make([]governanceCaseAuditTransition, 0, min(len(transitionRows), int(limit))),
		TransitionsTruncated: len(transitionRows) > int(limit),
		Attempts:             make([]governanceCaseAuditAttempt, 0, min(len(attemptRows), int(limit))),
		AttemptsTruncated:    len(attemptRows) > int(limit),
		Evaluations:          make([]governanceCaseAuditEvaluation, 0, min(len(evaluationRows), int(limit))),
		EvaluationsTruncated: len(evaluationRows) > int(limit),
	}
	if response.TransitionsTruncated {
		response.TransitionsNextCursor = governanceCaseAuditCursorFor(transitionRows[int(limit)-1].CreatedAt, transitionRows[int(limit)-1].ID)
	}
	if response.AttemptsTruncated {
		response.AttemptsNextCursor = governanceCaseAuditCursorFor(attemptRows[int(limit)-1].CreatedAt, attemptRows[int(limit)-1].ID)
	}
	if response.EvaluationsTruncated {
		last := evaluationRows[int(limit)-1]
		response.EvaluationsNextCursor = &governanceCaseAuditEvaluationCursor{
			CapturedAt: last.CapturedAt.Time.UTC().Format(time.RFC3339Nano),
			ID:         uuidToString(last.ID),
		}
	}
	for _, row := range transitionRows[:min(len(transitionRows), int(limit))] {
		response.Transitions = append(response.Transitions, governanceCaseAuditTransition{
			ID: uuidToString(row.ID), ResultingStateRevision: row.ResultingStateRevision,
			ExpectedStateRevision: row.ExpectedStateRevision, FromState: row.FromState, ToState: row.ToState,
			ActorType: row.ActorType, CreatedAt: timestampToString(row.CreatedAt),
		})
	}
	for _, row := range attemptRows[:min(len(attemptRows), int(limit))] {
		response.Attempts = append(response.Attempts, governanceCaseAuditAttempt{
			ID: uuidToString(row.ID), Ordinal: row.Ordinal, Kind: row.Kind,
			ObligationID: optionalUUID(row.ObligationID), DeadlineAt: timestampToPtr(row.DeadlineAt),
			TerminalAt: timestampToPtr(row.TerminalAt), CreatedAt: timestampToString(row.CreatedAt),
		})
	}
	for _, row := range evaluationRows[:min(len(evaluationRows), int(limit))] {
		response.Evaluations = append(response.Evaluations, governanceCaseAuditEvaluation{
			ID: uuidToString(row.ID), SnapshotSchemaVersion: row.SnapshotSchemaVersion,
			RequiredComplete: row.RequiredComplete, EstimatedTokens: row.EstimatedTokens,
			CapturedAt: timestampToString(row.CapturedAt), RedactedAt: timestampToPtr(row.RedactedAt),
		})
	}
	return response, true
}

func governanceCaseAuditQueryFailure(w http.ResponseWriter, workspaceID pgtype.UUID, action string, err error) (governanceCaseAuditResponse, bool) {
	slog.Error("governance case audit query failed", "workspace_id", uuidToString(workspaceID), "action", action, "error", err)
	writeError(w, http.StatusInternalServerError, "failed to load governance case audit")
	return governanceCaseAuditResponse{}, false
}

func optionalUUID(id pgtype.UUID) *string {
	if !id.Valid {
		return nil
	}
	value := uuidToString(id)
	return &value
}

func rowID(id pgtype.UUID) string {
	return uuidToString(id)
}
