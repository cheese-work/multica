package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/taskgateway"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func taskGatewayGrantRows(test *testing.T) (*Handler, db.AgentTaskQueue, db.AgentRuntime, db.Agent, db.TaskToken, context.Context) {
	test.Helper()
	workspaceID := "00000000-0000-4000-8000-000000000003"
	runtimeID := "00000000-0000-4000-8000-000000000004"
	ownerID := "00000000-0000-4000-8000-000000000002"
	taskID := "00000000-0000-4000-8000-000000000001"
	agentID := "00000000-0000-4000-8000-000000000005"
	operator, err := taskgateway.New("https://owned.example", "owned-operator-secret", []taskgateway.Policy{{RuntimeID: runtimeID, OwnerID: ownerID, WorkspaceID: workspaceID, TaskID: taskID, GatewayOwnerID: 7, Limit: 100, KeyIDs: map[string]int64{"claude": 11, "codex": 12}}})
	if err != nil {
		test.Fatal(err)
	}
	runtime := db.AgentRuntime{ID: parseUUID(runtimeID), OwnerID: parseUUID(ownerID), WorkspaceID: parseUUID(workspaceID), DaemonID: pgtype.Text{String: "owned-daemon", Valid: true}, Provider: "codex", Visibility: "private"}
	task := db.AgentTaskQueue{ID: parseUUID(taskID), RuntimeID: runtime.ID, AgentID: parseUUID(agentID), Status: "dispatched", DispatchedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}}
	agent := db.Agent{ID: task.AgentID, RuntimeID: runtime.ID, OwnerID: runtime.OwnerID}
	token := db.TaskToken{TaskID: task.ID, AgentID: task.AgentID, UserID: runtime.OwnerID, WorkspaceID: runtime.WorkspaceID, Purpose: "agent_task", ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}}
	ctx := middleware.WithDaemonContext(context.Background(), workspaceID, "owned-daemon")
	return &Handler{TaskGateway: operator}, task, runtime, agent, token, ctx
}

type taskGatewayGrantTx struct {
	pgx.Tx
	test       *testing.T
	task       db.AgentTaskQueue
	runtime    db.AgentRuntime
	agent      db.Agent
	token      db.TaskToken
	order      []string
	fail       string
	committed  bool
	rolledBack bool
}

func (tx *taskGatewayGrantTx) Begin(context.Context) (pgx.Tx, error) { return tx, nil }
func (tx *taskGatewayGrantTx) Commit(context.Context) error {
	if tx.fail == "commit" {
		return errors.New("owned commit refusal")
	}
	tx.committed = true
	return nil
}
func (tx *taskGatewayGrantTx) Rollback(context.Context) error { tx.rolledBack = true; return nil }

type taskGatewayGrantRow struct {
	value any
	err   error
}

func (row taskGatewayGrantRow) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	value := reflect.ValueOf(row.value)
	if len(destinations) != value.NumField() {
		return errors.New("owned row shape mismatch")
	}
	for index, destination := range destinations {
		target := reflect.ValueOf(destination).Elem()
		if target.Type() != value.Field(index).Type() {
			return errors.New("owned column type mismatch")
		}
		target.Set(value.Field(index))
	}
	return nil
}

func (tx *taskGatewayGrantTx) QueryRow(_ context.Context, query string, arguments ...any) pgx.Row {
	for _, candidate := range []struct {
		name      string
		value     any
		arguments []any
	}{
		{"LockAgentRuntime", tx.runtime, []any{tx.runtime.ID}},
		{"LockAgentTaskStartClaim", tx.task, []any{tx.task.ID, tx.runtime.ID, tx.task.DispatchedAt}},
		{"GetAgentForUpdate", tx.agent, []any{tx.task.AgentID}},
		{"LockTaskTokenByHash", tx.token, []any{tx.token.TokenHash}},
	} {
		if !strings.Contains(query, "-- name: "+candidate.name+" :one") {
			continue
		}
		tx.order = append(tx.order, candidate.name)
		if !reflect.DeepEqual(arguments, candidate.arguments) {
			tx.test.Error("grant query used another claim identity")
		}
		if tx.fail == candidate.name {
			return taskGatewayGrantRow{err: pgx.ErrNoRows}
		}
		if !strings.Contains(query, "FOR UPDATE") {
			tx.test.Error("grant query omitted authorization lock")
		}
		return taskGatewayGrantRow{value: candidate.value}
	}
	tx.test.Error("unexpected grant database query")
	return taskGatewayGrantRow{err: pgx.ErrNoRows}
}

func TestTaskGatewayGrantTransactionalDelivery(test *testing.T) {
	for _, refusal := range []string{"", "LockAgentRuntime", "LockAgentTaskStartClaim", "GetAgentForUpdate", "LockTaskTokenByHash", "commit", "operator", "old daemon", "expired token", "foreign daemon"} {
		test.Run(refusal, func(test *testing.T) {
			handler, task, runtime, agent, token, ctx := taskGatewayGrantRows(test)
			token.TokenHash = auth.HashToken("mat_owned-task-secret")
			if refusal == "expired token" {
				token.ExpiresAt.Time = time.Now().Add(-time.Second)
			}
			if refusal == "foreign daemon" {
				runtime.DaemonID.String = "foreign-daemon"
			}
			tx := &taskGatewayGrantTx{test: test, task: task, runtime: runtime, agent: agent, token: token, fail: refusal}
			handler.TxStarter = tx
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if request.Header.Get("X-Api-Key") != "owned-operator-secret" {
					test.Error("wrong trusted operator credential")
				}
				if refusal == "operator" {
					writer.WriteHeader(http.StatusTooManyRequests)
					return
				}
				var data string
				switch request.Method + " " + request.URL.Path {
				case "GET /api/v1/admin/users/7/api-keys":
					data = `{"items":[{"id":11,"user_id":7,"key":"owned-claude-secret","status":"active"},{"id":12,"user_id":7,"key":"owned-codex-secret","status":"active"}],"pages":1}`
				case "PUT /api/v1/admin/task-quotas/" + uuidToString(task.ID):
					data = `{"task_id":"` + uuidToString(task.ID) + `"}`
				case "GET /api/v1/admin/task-quotas/" + uuidToString(task.ID):
					data = `{"task_id":"` + uuidToString(task.ID) + `","closed":false,"reconciliation_required":false,"policy":{"version":1,"unit":"weighted_token_tenths","input_weight":10,"cache_creation_weight":10,"cache_read_weight":1,"output_weight":50},"exact":{"owner_id":"7","api_key_ids":["11","12"],"limit_tenths":"100","spent_tenths":"20","held_tenths":"0","remaining_tenths":"80"}}`
				default:
					test.Error("unexpected operator request")
				}
				_, _ = writer.Write([]byte(`{"code":0,"data":` + data + `}`))
			}))
			defer server.Close()
			var err error
			handler.TaskGateway, err = taskgateway.New(server.URL, "owned-operator-secret", []taskgateway.Policy{{RuntimeID: uuidToString(runtime.ID), OwnerID: uuidToString(runtime.OwnerID), WorkspaceID: uuidToString(runtime.WorkspaceID), TaskID: uuidToString(task.ID), GatewayOwnerID: 7, Limit: 100, KeyIDs: map[string]int64{"claude": 11, "codex": 12}}})
			if err != nil {
				test.Fatal(err)
			}
			route := chi.NewRouteContext()
			route.URLParams.Add("runtimeId", uuidToString(runtime.ID))
			route.URLParams.Add("taskId", uuidToString(task.ID))
			ctx = context.WithValue(ctx, chi.RouteCtxKey, route)
			request := httptest.NewRequest(http.MethodPost, "/owned-grant", strings.NewReader(`{"task_token":"mat_owned-task-secret","dispatched_at":"`+task.DispatchedAt.Time.Format(time.RFC3339Nano)+`"}`)).WithContext(ctx)
			if refusal != "old daemon" {
				request.Header.Set("X-Client-Capabilities", taskgateway.Capability)
			}
			writer := httptest.NewRecorder()
			handler.DeliverTaskGatewayGrant(writer, request)
			if writer.Header().Get("Cache-Control") != "no-store" || !tx.rolledBack {
				test.Fatal("grant caching or unfinished transaction")
			}
			if refusal == "" {
				binding := credentialexec.Binding{TaskID: uuidToString(task.ID), OwnerID: uuidToString(runtime.OwnerID), WorkspaceID: uuidToString(runtime.WorkspaceID)}
				grant, err := taskgateway.DecodeHandoff(writer.Body.Bytes(), binding)
				if writer.Code != http.StatusOK || err != nil || grant.Key != "owned-codex-secret" || !tx.committed || calls.Load() != 3 || !reflect.DeepEqual(tx.order, []string{"LockAgentRuntime", "LockAgentTaskStartClaim", "GetAgentForUpdate", "LockTaskTokenByHash"}) {
					test.Fatal("authenticated locked handoff failed")
				}
			} else {
				if writer.Code != http.StatusServiceUnavailable || tx.committed || strings.Contains(writer.Body.String(), "secret") {
					test.Fatal("refused handoff returned a credential or committed")
				}
				expected := int32(0)
				if refusal == "commit" {
					expected = 3
				}
				if refusal == "operator" {
					expected = 1
				}
				if calls.Load() != expected {
					test.Fatal("refused handoff reached or retried operator")
				}
			}
		})
	}
}

func TestTaskGatewayClaimDaemonToken(test *testing.T) {
	_, _, runtime, _, _, _ := taskGatewayGrantRows(test)
	raw, tokens, err := daemonTokenForClaim(AgentTaskResponse{}, runtime, true)
	if err != nil || !strings.HasPrefix(raw, "mdt_") || len(tokens) != 1 || tokens[0].TokenHash != auth.HashToken(raw) || tokens[0].DaemonID != runtime.DaemonID.String || tokens[0].WorkspaceID != runtime.WorkspaceID {
		test.Fatal("missing task-owned daemon token")
	}
	response := AgentTaskResponse{RequireCredentialIsolation: true}
	setClaimDaemonTokens(&response, raw)
	if response.TaskGatewayDaemonToken != raw || response.RemoteMCPDaemonToken != "" {
		test.Fatal("gateway token leaked into another response route")
	}
	runtime.DaemonID.Valid = false
	if _, _, err := daemonTokenForClaim(AgentTaskResponse{}, runtime, true); err == nil {
		test.Fatal("ownerless daemon token minted")
	}
	if raw, tokens, err := daemonTokenForClaim(AgentTaskResponse{}, runtime, false); err != nil || raw != "" || len(tokens) != 0 {
		test.Fatal("unprotected task got a gateway token")
	}
}

func TestTaskGatewayGrantRequiresOwningDaemonAndCommittedToken(test *testing.T) {
	handler, task, runtime, agent, token, ctx := taskGatewayGrantRows(test)
	binding, err := handler.authorizeTaskGatewayGrant(ctx, task, runtime, agent, token, []string{taskgateway.Capability})
	if err != nil || binding != (credentialexec.Binding{TaskID: uuidToString(task.ID), OwnerID: uuidToString(runtime.OwnerID), WorkspaceID: uuidToString(runtime.WorkspaceID)}) {
		test.Fatal("owned daemon and committed exact token denied")
	}
	for _, candidate := range []struct {
		name   string
		change func(*db.AgentTaskQueue, *db.AgentRuntime, *db.Agent, *db.TaskToken)
	}{
		{"changed task runtime", func(task *db.AgentTaskQueue, _ *db.AgentRuntime, _ *db.Agent, _ *db.TaskToken) {
			task.RuntimeID = task.ID
		}},
		{"changed agent runtime", func(_ *db.AgentTaskQueue, _ *db.AgentRuntime, agent *db.Agent, _ *db.TaskToken) {
			agent.RuntimeID = agent.ID
		}},
		{"changed runtime owner", func(_ *db.AgentTaskQueue, runtime *db.AgentRuntime, _ *db.Agent, _ *db.TaskToken) {
			runtime.OwnerID = runtime.ID
		}},
		{"changed private agent owner", func(_ *db.AgentTaskQueue, _ *db.AgentRuntime, agent *db.Agent, _ *db.TaskToken) {
			agent.OwnerID = agent.ID
		}},
		{"foreign daemon", func(_ *db.AgentTaskQueue, runtime *db.AgentRuntime, _ *db.Agent, _ *db.TaskToken) {
			runtime.DaemonID.String = "foreign-daemon"
		}},
		{"missing daemon", func(_ *db.AgentTaskQueue, runtime *db.AgentRuntime, _ *db.Agent, _ *db.TaskToken) {
			runtime.DaemonID.Valid = false
		}},
		{"foreign workspace", func(_ *db.AgentTaskQueue, runtime *db.AgentRuntime, _ *db.Agent, _ *db.TaskToken) {
			runtime.WorkspaceID = runtime.ID
		}},
		{"foreign token task", func(_ *db.AgentTaskQueue, _ *db.AgentRuntime, _ *db.Agent, token *db.TaskToken) {
			token.TaskID = token.AgentID
		}},
		{"foreign token agent", func(_ *db.AgentTaskQueue, _ *db.AgentRuntime, _ *db.Agent, token *db.TaskToken) {
			token.AgentID = token.TaskID
		}},
		{"foreign token owner", func(_ *db.AgentTaskQueue, _ *db.AgentRuntime, _ *db.Agent, token *db.TaskToken) {
			token.UserID = token.TaskID
		}},
		{"foreign token workspace", func(_ *db.AgentTaskQueue, _ *db.AgentRuntime, _ *db.Agent, token *db.TaskToken) {
			token.WorkspaceID = token.TaskID
		}},
		{"expired token", func(_ *db.AgentTaskQueue, _ *db.AgentRuntime, _ *db.Agent, token *db.TaskToken) {
			token.ExpiresAt.Time = time.Now().Add(-time.Second)
		}},
		{"governance token", func(_ *db.AgentTaskQueue, _ *db.AgentRuntime, _ *db.Agent, token *db.TaskToken) {
			token.Purpose = "governance_proposal"
		}},
		{"missing dispatch", func(task *db.AgentTaskQueue, _ *db.AgentRuntime, _ *db.Agent, _ *db.TaskToken) {
			task.DispatchedAt.Valid = false
		}},
		{"terminal task", func(task *db.AgentTaskQueue, _ *db.AgentRuntime, _ *db.Agent, _ *db.TaskToken) {
			task.Status = "completed"
		}},
		{"pending task", func(task *db.AgentTaskQueue, _ *db.AgentRuntime, _ *db.Agent, _ *db.TaskToken) {
			task.Status = "pending"
		}},
		{"custom profile", func(_ *db.AgentTaskQueue, runtime *db.AgentRuntime, _ *db.Agent, _ *db.TaskToken) {
			runtime.ProfileID = runtime.ID
		}},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			current, target, currentAgent, currentToken := task, runtime, agent, token
			candidate.change(&current, &target, &currentAgent, &currentToken)
			if _, err := handler.authorizeTaskGatewayGrant(ctx, current, target, currentAgent, currentToken, []string{taskgateway.Capability}); err == nil || strings.Contains(err.Error(), "owned-operator-secret") {
				test.Fatal("unsafe grant identity admitted")
			}
		})
	}
	for _, invalid := range []context.Context{context.Background(), middleware.WithDaemonContext(context.Background(), uuidToString(runtime.ID), "owned-daemon"), middleware.WithDaemonContext(context.Background(), uuidToString(runtime.WorkspaceID), "foreign-daemon")} {
		if _, err := handler.authorizeTaskGatewayGrant(invalid, task, runtime, agent, token, []string{taskgateway.Capability}); err == nil {
			test.Fatal("unauthenticated or foreign daemon admitted")
		}
	}
	if _, err := handler.authorizeTaskGatewayGrant(ctx, task, runtime, agent, token, nil); err == nil {
		test.Fatal("old daemon admitted")
	}
	task.Status = "running"
	if _, err := handler.authorizeTaskGatewayGrant(ctx, task, runtime, agent, token, []string{taskgateway.Capability}); err != nil {
		test.Fatal("same-task running resume denied")
	}
}
