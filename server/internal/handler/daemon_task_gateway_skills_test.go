package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/taskgateway"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type taskGatewaySkillDB struct {
	db.DBTX
	test    *testing.T
	task    db.AgentTaskQueue
	runtime db.AgentRuntime
	agent   db.Agent
	fail    string
}

func (fixture *taskGatewaySkillDB) QueryRow(_ context.Context, query string, arguments ...any) pgx.Row {
	for _, candidate := range []struct {
		name  string
		value any
		id    any
	}{
		{"GetAgentRuntime", fixture.runtime, fixture.runtime.ID},
		{"GetAgentTask", fixture.task, fixture.task.ID},
		{"GetAgent", fixture.agent, fixture.task.AgentID},
	} {
		if !strings.Contains(query, "-- name: "+candidate.name+" :one") {
			continue
		}
		if !reflect.DeepEqual(arguments, []any{candidate.id}) {
			fixture.test.Error("skill resolution queried another identity")
		}
		if fixture.fail == candidate.name {
			return taskGatewayGrantRow{err: pgx.ErrNoRows}
		}
		return taskGatewayGrantRow{value: candidate.value}
	}
	fixture.test.Error("unexpected skill-resolution query")
	return taskGatewayGrantRow{err: pgx.ErrNoRows}
}

type taskGatewaySkillAuthContext struct{ context.Context }

func (ctx taskGatewaySkillAuthContext) Value(key any) any {
	value := ctx.Context.Value(key)
	if label, ok := value.(string); ok && label == middleware.DaemonAuthPathDaemonToken {
		return middleware.DaemonAuthPathPAT
	}
	return value
}

func TestTaskGatewayRunningSkillResolution(test *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		test.Run(provider, func(test *testing.T) {
			for _, candidate := range []string{
				"running", "unmanaged dispatched", "unmanaged waiting", "unmanaged running", "old daemon",
				"PAT", "foreign daemon", "missing daemon", "missing daemon ID", "changed owner", "missing owner", "changed workspace",
				"changed task", "moved task", "moved agent", "changed agent", "foreign private agent", "custom profile",
				"missing dispatch", "infinite dispatch", "unavailable agent", "unsupported provider", "completed", "queued", "cancelled",
			} {
				test.Run(candidate, func(test *testing.T) {
					handler, task, runtime, agent, _, ctx := taskGatewayGrantRows(test)
					runtime.Provider = provider
					task.Status = "running"
					operator := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
						test.Error("skill resolution attempted an operator request")
						writer.WriteHeader(http.StatusServiceUnavailable)
					}))
					defer operator.Close()
					var err error
					handler.TaskGateway, err = taskgateway.New(operator.URL, "owned-operator-secret", []taskgateway.Policy{{RuntimeID: uuidToString(runtime.ID), OwnerID: uuidToString(runtime.OwnerID), WorkspaceID: uuidToString(runtime.WorkspaceID), TaskID: uuidToString(task.ID), GatewayOwnerID: 7, Limit: 100, KeyIDs: map[string]int64{"claude": 11, "codex": 12}}})
					if err != nil {
						test.Fatal(err)
					}
					capability := taskgateway.Capability
					other := parseUUID("00000000-0000-4000-8000-000000000006")
					failure := ""
					wantAllowed := false
					switch candidate {
					case "running":
						wantAllowed = true
					case "unmanaged dispatched", "unmanaged waiting", "unmanaged running":
						handler.TaskGateway = nil
						capability = ""
						if candidate == "unmanaged dispatched" {
							task.Status, wantAllowed = "dispatched", true
						} else if candidate == "unmanaged waiting" {
							task.Status, wantAllowed = "waiting_local_directory", true
						}
					case "old daemon":
						capability = ""
					case "PAT":
						ctx = taskGatewaySkillAuthContext{ctx}
					case "foreign daemon":
						runtime.DaemonID.String = "foreign-daemon"
					case "missing daemon":
						runtime.DaemonID.Valid = false
					case "missing daemon ID":
						ctx = middleware.WithDaemonContext(ctx, uuidToString(runtime.WorkspaceID), "")
					case "changed owner":
						runtime.OwnerID = other
					case "missing owner":
						runtime.OwnerID.Valid = false
					case "changed workspace":
						runtime.WorkspaceID = other
					case "changed task":
						task.ID = other
					case "moved task":
						task.RuntimeID = other
					case "moved agent":
						agent.RuntimeID = other
					case "changed agent":
						agent.ID = other
					case "foreign private agent":
						agent.OwnerID = other
					case "custom profile":
						runtime.ProfileID = other
					case "missing dispatch":
						task.DispatchedAt.Valid = false
					case "infinite dispatch":
						task.DispatchedAt.InfinityModifier = pgtype.Infinity
					case "unavailable agent":
						failure = "GetAgent"
					case "unsupported provider":
						runtime.Provider = "unsupported"
					default:
						task.Status = candidate
					}
					task.Context, _ = json.Marshal(service.QuickCreateContext{Type: service.QuickCreateContextType, WorkspaceID: uuidToString(runtime.WorkspaceID)})
					fixture := &taskGatewaySkillDB{test: test, task: task, runtime: runtime, agent: agent, fail: failure}
					handler.Queries = db.New(fixture)
					handler.TaskService = &service.TaskService{Queries: handler.Queries}
					request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"skills":[]}`)).WithContext(ctx)
					request = withURLParams(request, "runtimeId", uuidToString(runtime.ID), "taskId", uuidToString(task.ID))
					request.Header.Set("X-Client-Capabilities", capability)
					writer := httptest.NewRecorder()
					handler.ResolveTaskSkillBundles(writer, request)
					if (writer.Code == http.StatusOK) != wantAllowed {
						test.Fatalf("resolution status %d, want allowed %v", writer.Code, wantAllowed)
					}
					if wantAllowed && strings.TrimSpace(writer.Body.String()) != `{"bundles":[]}` {
						test.Fatal("empty-resolution contract changed")
					}
				})
			}
		})
	}
}
