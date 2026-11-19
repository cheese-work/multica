package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/logger"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
)

// CLI update request store

type UpdateStatus string

const (
	UpdatePending   UpdateStatus = "pending"
	UpdateRunning   UpdateStatus = "running"
	UpdateCompleted UpdateStatus = "completed"
	UpdateFailed    UpdateStatus = "failed"
	UpdateTimeout   UpdateStatus = "timeout"
	UpdateCancelled UpdateStatus = "cancelled"
)

// UpdateProgress is the daemon's latest self-reported download state for a
// running update.
type UpdateProgress struct {
	Phase      string `json:"phase"`
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
}

// UpdateRequest represents a pending or completed CLI update request.
type UpdateRequest struct {
	ID              string          `json:"id"`
	RuntimeID       string          `json:"runtime_id"`
	InitiatorUserID string          `json:"-"`
	Status          UpdateStatus    `json:"status"`
	TargetVersion   string          `json:"target_version"`
	Output          string          `json:"output,omitempty"`
	Error           string          `json:"error,omitempty"`
	Progress        *UpdateProgress `json:"progress,omitempty"`
	// CycleID is set for a scheduled-update cycle's request; empty for a
	// manual update.
	CycleID      string     `json:"cycle_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	RunStartedAt *time.Time `json:"-"`
}

const (
	updatePendingTimeout = 120 * time.Second
	updateRunningTimeout = 150 * time.Second
	updateStoreRetention = 5 * time.Minute
)

type UpdateStore interface {
	Create(ctx context.Context, runtimeID, targetVersion, initiatorUserID string) (*UpdateRequest, error)
	// CreateWithID is Create with a caller-given request id (a cycle uses
	// "<cycle_id>-<attempt>"). A duplicate id is errUpdateDuplicateID.
	CreateWithID(ctx context.Context, id, cycleID, runtimeID, targetVersion, initiatorUserID string) (*UpdateRequest, error)
	// Cancel moves a pending request to cancelled atomically. False when the
	// request is running, terminal or unknown.
	Cancel(ctx context.Context, id string) (bool, error)
	Get(ctx context.Context, id string) (*UpdateRequest, error)
	HasPending(ctx context.Context, runtimeID string) (bool, error)
	PopPending(ctx context.Context, runtimeID string) (*UpdateRequest, error)
	Complete(ctx context.Context, id string, output string) error
	Fail(ctx context.Context, id string, errMsg string) error
	// SetProgress records progress on a running request; any other status is
	// a no-op so a late report cannot touch a settled request.
	SetProgress(ctx context.Context, id string, progress UpdateProgress) error
}

func updateRequestTerminal(status UpdateStatus) bool {
	return status == UpdateCompleted || status == UpdateFailed || status == UpdateTimeout || status == UpdateCancelled
}

func applyUpdateTimeout(req *UpdateRequest, now time.Time) bool {
	switch req.Status {
	case UpdatePending:
		if now.Sub(req.CreatedAt) > updatePendingTimeout {
			req.Status = UpdateTimeout
			req.Error = "daemon did not respond within 120 seconds"
			req.UpdatedAt = now
			return true
		}
	case UpdateRunning:
		if req.RunStartedAt != nil && now.Sub(*req.RunStartedAt) > updateRunningTimeout {
			req.Status = UpdateTimeout
			req.Error = "update did not complete within 150 seconds"
			req.UpdatedAt = now
			return true
		}
	}
	return false
}

// InMemoryUpdateStore is the single-node implementation. Multi-node deploys
// must use RedisUpdateStore so Web POST, daemon heartbeat, daemon report, and
// UI polling agree on the same request lifecycle.
type InMemoryUpdateStore struct {
	mu       sync.Mutex
	requests map[string]*UpdateRequest // keyed by update ID
}

func NewInMemoryUpdateStore() *InMemoryUpdateStore {
	return &InMemoryUpdateStore{
		requests: make(map[string]*UpdateRequest),
	}
}

func (s *InMemoryUpdateStore) Create(ctx context.Context, runtimeID, targetVersion, initiatorUserID string) (*UpdateRequest, error) {
	return s.CreateWithID(ctx, randomID(), "", runtimeID, targetVersion, initiatorUserID)
}

func (s *InMemoryUpdateStore) CreateWithID(_ context.Context, id, cycleID, runtimeID, targetVersion, initiatorUserID string) (*UpdateRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Clean up old requests.
	for id, req := range s.requests {
		if time.Since(req.CreatedAt) > updateStoreRetention {
			delete(s.requests, id)
		}
	}

	if _, dup := s.requests[id]; dup {
		return nil, errUpdateDuplicateID
	}
	// Reject if there is already a pending or running update for this runtime.
	for _, req := range s.requests {
		if req.RuntimeID == runtimeID && (req.Status == UpdatePending || req.Status == UpdateRunning) {
			return nil, errUpdateInProgress
		}
	}

	req := &UpdateRequest{
		ID:              id,
		CycleID:         cycleID,
		RuntimeID:       runtimeID,
		InitiatorUserID: initiatorUserID,
		Status:          UpdatePending,
		TargetVersion:   targetVersion,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	s.requests[req.ID] = req
	return req, nil
}

var errUpdateInProgress = &updateError{msg: "an update is already in progress for this runtime"}

var errUpdateDuplicateID = &updateError{msg: "update request id already exists"}

type updateError struct{ msg string }

func (e *updateError) Error() string { return e.msg }

func (s *InMemoryUpdateStore) Cancel(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.requests[id]
	if !ok {
		return false, nil
	}
	now := time.Now()
	applyUpdateTimeout(req, now)
	if req.Status != UpdatePending {
		return false, nil
	}
	req.Status = UpdateCancelled
	req.UpdatedAt = now
	return true, nil
}

func (s *InMemoryUpdateStore) Get(_ context.Context, id string) (*UpdateRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.requests[id]
	if !ok {
		return nil, nil
	}
	applyUpdateTimeout(req, time.Now())
	return req, nil
}

func (s *InMemoryUpdateStore) HasPending(_ context.Context, runtimeID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for _, req := range s.requests {
		applyUpdateTimeout(req, now)
		if req.RuntimeID == runtimeID && req.Status == UpdatePending {
			return true, nil
		}
	}
	return false, nil
}

// PopPending returns and marks as running the pending update for a runtime.
func (s *InMemoryUpdateStore) PopPending(_ context.Context, runtimeID string) (*UpdateRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var oldest *UpdateRequest
	now := time.Now()
	for _, req := range s.requests {
		applyUpdateTimeout(req, now)
		if req.RuntimeID == runtimeID && req.Status == UpdatePending {
			if oldest == nil || req.CreatedAt.Before(oldest.CreatedAt) {
				oldest = req
			}
		}
	}
	if oldest != nil {
		oldest.Status = UpdateRunning
		startedAt := now
		oldest.RunStartedAt = &startedAt
		oldest.UpdatedAt = now
	}
	return oldest, nil
}

func (s *InMemoryUpdateStore) Complete(_ context.Context, id string, output string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req, ok := s.requests[id]; ok {
		req.Status = UpdateCompleted
		req.Output = output
		req.UpdatedAt = time.Now()
	}
	return nil
}

func (s *InMemoryUpdateStore) Fail(_ context.Context, id string, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req, ok := s.requests[id]; ok {
		req.Status = UpdateFailed
		req.Error = errMsg
		req.UpdatedAt = time.Now()
	}
	return nil
}

func (s *InMemoryUpdateStore) SetProgress(_ context.Context, id string, progress UpdateProgress) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req, ok := s.requests[id]; ok && req.Status == UpdateRunning {
		req.Progress = &progress
		req.UpdatedAt = time.Now()
	}
	return nil
}

// Handlers

// InitiateUpdate creates a new CLI update request (protected route, called by frontend).
func (h *Handler) InitiateUpdate(w http.ResponseWriter, r *http.Request) {
	runtimeID := chi.URLParam(r, "runtimeId")
	rt, member, ok := h.requireRuntimeReadAccess(w, r, obsmetrics.RuntimeLookupSourceRuntimeAPI, runtimeID)
	if !ok {
		return
	}
	if !canEditRuntime(member, rt) {
		writeError(w, http.StatusForbidden, "only runtime owners and workspace admins can update runtimes")
		return
	}

	var req struct {
		TargetVersion string `json:"target_version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TargetVersion == "" {
		writeError(w, http.StatusBadRequest, "target_version is required")
		return
	}

	var update *UpdateRequest
	err := h.auditThenEnqueue(r.Context(), r, auditInput{
		Kind: "runtime.update", WorkspaceID: rt.WorkspaceID, TargetID: rt.ID,
		After: map[string]any{"target_version": req.TargetVersion},
	}, func() (err error) {
		update, err = h.UpdateStore.Create(
			r.Context(),
			uuidToString(rt.ID),
			req.TargetVersion,
			uuidToString(member.UserID),
		)
		return err
	})
	if failedAudit(w, r, err, "runtime.update") {
		return
	}
	if err != nil {
		// Only the in-progress rejection is a conflict the caller can act on.
		// Every other Create failure is infrastructure — the Redis store wraps
		// connection failures as "reserve active update: ..." / "persist update
		// request: ..." — and echoing it back would both leak internals and
		// label an outage as a user-fixable conflict. That was survivable while
		// the CLI hid 409 bodies; it no longer is, now that they are shown by
		// default.
		if errors.Is(err, errUpdateInProgress) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		slog.Error("UpdateStore Create failed", "error", err, "runtime_id", uuidToString(rt.ID))
		writeError(w, http.StatusInternalServerError, "failed to start the update")
		return
	}

	writeJSON(w, http.StatusOK, update)
}

// GetUpdate returns the status of an update request (protected route, called by frontend).
func (h *Handler) GetUpdate(w http.ResponseWriter, r *http.Request) {
	runtimeID := chi.URLParam(r, "runtimeId")
	rt, member, ok := h.requireRuntimeReadAccess(w, r, obsmetrics.RuntimeLookupSourceRuntimeUpdatePoll, runtimeID)
	if !ok {
		return
	}
	updateID := chi.URLParam(r, "updateId")

	update, err := h.UpdateStore.Get(r.Context(), updateID)
	if err != nil {
		slog.Warn("load update failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to load update")
		return
	}
	if update == nil || update.RuntimeID != uuidToString(rt.ID) {
		writeError(w, http.StatusNotFound, "update not found")
		return
	}
	// Keep an in-flight poll alive if an admin is downgraded after starting
	// the update. This exception is scoped to the immutable request initiator;
	// other plain members still cannot read another runtime's update status.
	if !canEditRuntime(member, rt) && update.InitiatorUserID != uuidToString(member.UserID) {
		writeError(w, http.StatusForbidden, "only runtime owners, workspace admins, and the update initiator can view this update")
		return
	}

	writeJSON(w, http.StatusOK, update)
}

// cycleRequestID reports whether id has the form "<cycle_id>-<attempt>" a
// scheduled cycle gives its requests. Manual ids are dash-free hex.
func cycleRequestID(id string) bool {
	i := strings.LastIndexByte(id, '-')
	if i <= 0 || i == len(id)-1 {
		return false
	}
	for _, c := range id[i+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ReportUpdateResult receives the update result from the daemon.
func (h *Handler) ReportUpdateResult(w http.ResponseWriter, r *http.Request) {
	runtimeID := chi.URLParam(r, "runtimeId")

	// Verify the caller owns this runtime's workspace.
	rt, ok := h.requireDaemonRuntimeAccess(w, r, runtimeID)
	if !ok {
		return
	}

	updateID := chi.URLParam(r, "updateId")

	existing, err := h.UpdateStore.Get(r.Context(), updateID)
	if err != nil {
		slog.Warn("load update failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to load update")
		return
	}
	if existing != nil && existing.RuntimeID != runtimeID {
		writeError(w, http.StatusNotFound, "update not found")
		return
	}
	// A request of a scheduled cycle reports to the result sink, also when the
	// store already settled it or lost it (restart). Runtime and machine come
	// from the authenticated runtime, never from the body.
	cycle := h.UpdateResultSink != nil && (existing != nil && existing.CycleID != "" || existing == nil && cycleRequestID(updateID))
	if existing == nil && !cycle {
		writeError(w, http.StatusNotFound, "update not found")
		return
	}

	var req struct {
		Status   string          `json:"status"` // "running", "completed", or "failed"
		Reason   string          `json:"reason"`
		Output   string          `json:"output"`
		Error    string          `json:"error"`
		Progress *UpdateProgress `json:"progress"`
	}
	decode := func() bool {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return false
		}
		return true
	}

	if cycle {
		if !decode() {
			return
		}
		if req.Status == "completed" || req.Status == "failed" {
			outcome, err := h.UpdateResultSink.Record(r.Context(), ResultReport{
				RequestID: updateID, RuntimeID: runtimeID,
				OwnerID: uuidToString(rt.OwnerID), DaemonID: rt.DaemonID.String,
				Status: UpdateStatus(req.Status), Reason: req.Reason, Output: req.Output, Error: req.Error,
			})
			if err != nil {
				slog.Error("UpdateResultSink Record failed", "error", err, "update_id", updateID)
				writeError(w, http.StatusInternalServerError, "failed to record result")
				return
			}
			switch outcome {
			case RecordRecorded, RecordDuplicate:
			case RecordConflict:
				writeError(w, http.StatusConflict, "update result conflicts with the recorded result")
				return
			default: // RecordNotFound and any unknown outcome
				writeError(w, http.StatusNotFound, "update not found")
				return
			}
		}
		if existing == nil {
			if req.Status == "completed" || req.Status == "failed" {
				writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			} else {
				writeError(w, http.StatusNotFound, "update not found")
			}
			return
		}
	}

	if updateRequestTerminal(existing.Status) {
		slog.Debug("ignoring stale update report", "runtime_id", runtimeID, "update_id", updateID, "status", existing.Status)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !cycle && !decode() {
		return
	}
	switch req.Status {
	case "completed":
		if err := h.UpdateStore.Complete(r.Context(), updateID, req.Output); err != nil {
			slog.Error("UpdateStore Complete failed", "error", err, "update_id", updateID)
			writeError(w, http.StatusInternalServerError, "failed to persist completion")
			return
		}
	case "failed":
		if err := h.UpdateStore.Fail(r.Context(), updateID, req.Error); err != nil {
			slog.Error("UpdateStore Fail failed", "error", err, "update_id", updateID)
			writeError(w, http.StatusInternalServerError, "failed to persist failure")
			return
		}
	case "running":
		// Status is already "running" from PopPending; this call confirms the
		// daemon received the command and optionally carries download progress.
		if req.Progress != nil {
			if err := h.UpdateStore.SetProgress(r.Context(), updateID, *req.Progress); err != nil {
				slog.Error("UpdateStore SetProgress failed", "error", err, "update_id", updateID)
				writeError(w, http.StatusInternalServerError, "failed to persist progress")
				return
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "invalid status: "+req.Status)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// UpdateAdmission is the fence's verdict for one cycle request at delivery.
type UpdateAdmission struct {
	Allowed bool
	Reason  string
	// StartCutoff and DeadlineAt bound the daemon's start and finish; set when Allowed.
	StartCutoff time.Time
	DeadlineAt  time.Time
}

// UpdateFence decides, at heartbeat delivery, whether a cycle's request may start.
type UpdateFence interface {
	Admit(ctx context.Context, cycleID string) (UpdateAdmission, error)
}

// ResultReport is one terminal daemon result for a cycle request. Runtime and
// machine come from the authenticated URL runtime, never from the body.
type ResultReport struct {
	RequestID string
	RuntimeID string
	OwnerID   string
	DaemonID  string
	Status    UpdateStatus // completed or failed
	Reason    string
	Output    string
	Error     string
}

type RecordOutcome string

const (
	RecordRecorded  RecordOutcome = "recorded"
	RecordDuplicate RecordOutcome = "duplicate"
	RecordConflict  RecordOutcome = "conflict"
	RecordNotFound  RecordOutcome = "not_found"
)

type UpdateResultSink interface {
	Record(ctx context.Context, r ResultReport) (RecordOutcome, error)
}
