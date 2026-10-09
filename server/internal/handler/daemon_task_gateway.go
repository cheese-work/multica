package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/taskgateway"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (handler *Handler) authorizeTaskGatewayRuntimeTask(ctx context.Context, task db.AgentTaskQueue, runtime db.AgentRuntime, agent db.Agent, capabilities []string) (credentialexec.Binding, error) {
	binding := credentialExecutionBindingForClaim(task, runtime, middleware.DaemonWorkspaceIDFromContext(ctx))
	if middleware.DaemonAuthPathFromContext(ctx) != middleware.DaemonAuthPathDaemonToken || binding == nil || uuidToString(runtime.WorkspaceID) != binding.WorkspaceID || runtime.ProfileID.Valid || !runtime.DaemonID.Valid || runtime.DaemonID.String == "" || runtime.DaemonID.String != middleware.DaemonIDFromContext(ctx) || task.RuntimeID != runtime.ID || agent.ID != task.AgentID || agent.RuntimeID != runtime.ID || runtime.Visibility == "private" && agent.OwnerID != runtime.OwnerID || task.Status != "dispatched" && task.Status != "running" || !task.DispatchedAt.Valid || task.DispatchedAt.InfinityModifier != pgtype.Finite {
		return credentialexec.Binding{}, taskgateway.ErrUnavailable
	}
	required, err := handler.TaskGateway.Admit(uuidToString(runtime.ID), runtime.Provider, *binding, capabilities)
	if !required || err != nil {
		return credentialexec.Binding{}, taskgateway.ErrUnavailable
	}
	return *binding, nil
}

func (handler *Handler) authorizeTaskGatewayGrant(ctx context.Context, task db.AgentTaskQueue, runtime db.AgentRuntime, agent db.Agent, token db.TaskToken, capabilities []string) (credentialexec.Binding, error) {
	binding, err := handler.authorizeTaskGatewayRuntimeTask(ctx, task, runtime, agent, capabilities)
	if err != nil || token.TaskID != task.ID || token.AgentID != task.AgentID || token.UserID != runtime.OwnerID || token.WorkspaceID != runtime.WorkspaceID || token.Purpose != "agent_task" || !token.ExpiresAt.Valid || token.ExpiresAt.InfinityModifier != pgtype.Finite || !token.ExpiresAt.Time.After(time.Now()) {
		return credentialexec.Binding{}, taskgateway.ErrUnavailable
	}
	return binding, nil
}

type taskGatewayClaim struct {
	queries *db.Queries
	runtime db.AgentRuntime
	task    db.AgentTaskQueue
	agent   db.Agent
	binding credentialexec.Binding
}

func (handler *Handler) DeliverTaskGatewayGrant(writer http.ResponseWriter, request *http.Request) {
	handler.deliverTaskGatewayClaim(writer, request, func(ctx context.Context, claim taskGatewayClaim) ([]byte, error) {
		grant, err := handler.TaskGateway.Provision(ctx, uuidToString(claim.runtime.ID), claim.runtime.Provider, claim.binding)
		if err != nil {
			return nil, taskgateway.ErrUnavailable
		}
		return taskgateway.EncodeHandoff(grant)
	})
}

func (handler *Handler) deliverTaskGatewayClaim(writer http.ResponseWriter, request *http.Request, build func(context.Context, taskGatewayClaim) ([]byte, error)) {
	writer.Header().Set("Cache-Control", "no-store")
	refuse := func() { writeError(writer, http.StatusServiceUnavailable, "trusted task gateway unavailable") }
	if middleware.DaemonAuthPathFromContext(request.Context()) != middleware.DaemonAuthPathDaemonToken || handler.TaskGateway == nil || handler.TxStarter == nil {
		refuse()
		return
	}
	for _, name := range []string{"runtimeId", "taskId"} {
		value := chi.URLParam(request, name)
		identity, err := uuid.Parse(value)
		if err != nil || identity == uuid.Nil || identity.String() != value {
			refuse()
			return
		}
	}
	var payload struct {
		TaskToken    string `json:"task_token"`
		DispatchedAt string `json:"dispatched_at"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || !strings.HasPrefix(payload.TaskToken, "mat_") || strings.ContainsAny(payload.TaskToken, "\r\n\x00") {
		refuse()
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		refuse()
		return
	}
	dispatched, err := time.Parse(time.RFC3339Nano, payload.DispatchedAt)
	if err != nil {
		refuse()
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 25*time.Second)
	defer cancel()
	tx, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		refuse()
		return
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)
	runtime, err := queries.LockAgentRuntime(ctx, parseUUID(chi.URLParam(request, "runtimeId")))
	if err != nil || !runtime.DaemonID.Valid || runtime.DaemonID.String != middleware.DaemonIDFromContext(ctx) || uuidToString(runtime.WorkspaceID) != middleware.DaemonWorkspaceIDFromContext(ctx) {
		refuse()
		return
	}
	task, err := queries.LockAgentTaskStartClaim(ctx, db.LockAgentTaskStartClaimParams{ID: parseUUID(chi.URLParam(request, "taskId")), RuntimeID: runtime.ID, DispatchedAt: pgtype.Timestamptz{Time: dispatched, Valid: true}})
	if err != nil {
		refuse()
		return
	}
	agent, err := queries.GetAgentForUpdate(ctx, task.AgentID)
	if err != nil {
		refuse()
		return
	}
	token, err := queries.LockTaskTokenByHash(ctx, auth.HashToken(payload.TaskToken))
	if err != nil {
		refuse()
		return
	}
	binding, err := handler.authorizeTaskGatewayGrant(ctx, task, runtime, agent, token, requestClientCapabilities(request))
	if err != nil {
		refuse()
		return
	}
	contents, err := build(ctx, taskGatewayClaim{queries: queries, runtime: runtime, task: task, agent: agent, binding: binding})
	if err != nil {
		refuse()
		return
	}
	if _, err := handler.authorizeTaskGatewayGrant(ctx, task, runtime, agent, token, requestClientCapabilities(request)); err != nil {
		refuse()
		return
	}
	if ctx.Err() != nil {
		refuse()
		return
	}
	if tx.Commit(ctx) != nil {
		refuse()
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(contents)
}
