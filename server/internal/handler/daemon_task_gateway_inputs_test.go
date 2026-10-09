package handler

import (
	"context"
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
	"github.com/multica-ai/multica/server/internal/taskgateway"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type taskGatewayInputsTx struct {
	*taskGatewayGrantTx
	workspace db.Workspace
}

func (tx *taskGatewayInputsTx) Begin(context.Context) (pgx.Tx, error) { return tx, nil }

func (tx *taskGatewayInputsTx) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	if strings.Contains(query, "-- name: GetWorkspace :one") {
		tx.order = append(tx.order, "GetWorkspace")
		if !reflect.DeepEqual(arguments, []any{tx.runtime.WorkspaceID}) {
			tx.test.Error("refresh read another workspace")
		}
		if tx.fail == "GetWorkspace" {
			return taskGatewayGrantRow{err: pgx.ErrNoRows}
		}
		return taskGatewayGrantRow{value: tx.workspace}
	}
	return tx.taskGatewayGrantTx.QueryRow(ctx, query, arguments...)
}

func TestTaskGatewayInputSnapshotTransactionalDelivery(test *testing.T) {
	for _, refusal := range []string{"", "running", "LockAgentRuntime", "LockAgentTaskStartClaim", "GetAgentForUpdate", "LockTaskTokenByHash", "GetWorkspace", "commit", "old daemon", "expired token", "foreign daemon", "foreign owner", "foreign workspace", "terminal", "custom configuration", "oversize", "unknown field"} {
		test.Run(refusal, func(test *testing.T) {
			handler, task, runtime, agent, token, ctx := taskGatewayGrantRows(test)
			token.TokenHash = auth.HashToken("mat_owned-task-secret")
			agent.Instructions = "authenticated new instructions"
			workspace := db.Workspace{ID: runtime.WorkspaceID, Context: pgtype.Text{String: "authenticated workspace context", Valid: true}}
			switch refusal {
			case "running":
				task.Status = "running"
			case "expired token":
				token.ExpiresAt.Time = time.Now().Add(-time.Second)
			case "foreign daemon":
				runtime.DaemonID.String = "foreign"
			case "foreign owner":
				token.UserID = parseUUID("00000000-0000-4000-8000-000000000009")
			case "foreign workspace":
				workspace.ID = parseUUID("00000000-0000-4000-8000-000000000009")
			case "terminal":
				task.Status = "completed"
			case "custom configuration":
				agent.CustomEnv = []byte(`{"OPENAI_API_KEY":"owned-unlimited-secret"}`)
			case "oversize":
				agent.Instructions = strings.Repeat("X", (1<<20)+1)
			}
			tx := &taskGatewayInputsTx{taskGatewayGrantTx: &taskGatewayGrantTx{test: test, task: task, runtime: runtime, agent: agent, token: token, fail: refusal}, workspace: workspace}
			handler.TxStarter = tx
			var operatorCalls atomic.Int32
			operator := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				operatorCalls.Add(1)
				writer.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer operator.Close()
			var err error
			handler.TaskGateway, err = taskgateway.New(operator.URL, "owned-operator-secret", []taskgateway.Policy{{RuntimeID: uuidToString(runtime.ID), OwnerID: uuidToString(runtime.OwnerID), WorkspaceID: uuidToString(runtime.WorkspaceID), TaskID: uuidToString(task.ID), GatewayOwnerID: 7, Limit: 100, KeyIDs: map[string]int64{"claude": 11, "codex": 12}}})
			if err != nil {
				test.Fatal(err)
			}
			route := chi.NewRouteContext()
			route.URLParams.Add("runtimeId", uuidToString(runtime.ID))
			route.URLParams.Add("taskId", uuidToString(task.ID))
			ctx = context.WithValue(ctx, chi.RouteCtxKey, route)
			body := `{"task_token":"mat_owned-task-secret","dispatched_at":"` + task.DispatchedAt.Time.Format(time.RFC3339Nano) + `"}`
			if refusal == "unknown field" {
				body = strings.TrimSuffix(body, "}") + `,"instructions":"forged"}`
			}
			request := httptest.NewRequest(http.MethodPost, "/owned-inputs", strings.NewReader(body)).WithContext(ctx)
			if refusal != "old daemon" {
				request.Header.Set("X-Client-Capabilities", taskgateway.Capability)
			}
			writer := httptest.NewRecorder()
			handler.DeliverTaskGatewayInputs(writer, request)
			if writer.Header().Get("Cache-Control") != "no-store" || operatorCalls.Load() != 0 {
				test.Fatal("refresh cached data or reached the operator")
			}
			if refusal == "" || refusal == "running" {
				snapshot, err := taskgateway.DecodeInputSnapshot(writer.Body.Bytes())
				if writer.Code != http.StatusOK || err != nil || snapshot.Instructions != agent.Instructions || snapshot.WorkspaceContext != workspace.Context.String || snapshot.Binding.OwnerID != uuidToString(runtime.OwnerID) || !tx.committed || !tx.rolledBack || !reflect.DeepEqual(tx.order, []string{"LockAgentRuntime", "LockAgentTaskStartClaim", "GetAgentForUpdate", "LockTaskTokenByHash", "GetWorkspace"}) {
					test.Fatal("refresh did not deliver committed authenticated inputs", writer.Code, err)
				}
			} else if writer.Code != http.StatusServiceUnavailable || tx.committed || strings.Contains(writer.Body.String(), "secret") {
				test.Fatal("refused refresh committed or exposed credentials")
			}
		})
	}
}
