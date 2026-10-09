package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/taskgateway"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestTaskGatewayClaimUsesLockedIdentityAndCapability(test *testing.T) {
	taskID := "00000000-0000-4000-8000-000000000001"
	ownerID := "00000000-0000-4000-8000-000000000002"
	workspaceID := "00000000-0000-4000-8000-000000000003"
	runtimeID := "00000000-0000-4000-8000-000000000004"
	otherID := "00000000-0000-4000-8000-000000000005"
	operator, err := taskgateway.New("https://owned-gateway.example", "owned-operator-secret", []taskgateway.Policy{{RuntimeID: runtimeID, OwnerID: ownerID, WorkspaceID: workspaceID, TaskID: taskID, GatewayOwnerID: 7, Limit: 100, KeyIDs: map[string]int64{"claude": 11, "codex": 12}}})
	if err != nil {
		test.Fatal(err)
	}
	handler := &Handler{TaskGateway: operator}
	task := db.AgentTaskQueue{ID: parseUUID(taskID), RuntimeID: parseUUID(runtimeID), OriginatorUserID: parseUUID(otherID)}
	runtime := db.AgentRuntime{ID: parseUUID(runtimeID), OwnerID: parseUUID(ownerID), Provider: "codex"}
	response := AgentTaskResponse{WorkspaceID: workspaceID}
	if err := handler.applyTaskGatewayClaimPolicy(task, runtime, workspaceID, &response, []string{taskgateway.Capability}); err != nil || !response.RequireCredentialIsolation {
		test.Fatal("trusted capability denied")
	}
	if response.CredentialExecutionBinding == nil || *response.CredentialExecutionBinding != (credentialexec.Binding{TaskID: taskID, OwnerID: ownerID, WorkspaceID: workspaceID}) {
		test.Fatal("claim used originator or stale owner instead of locked runtime owner")
	}
	for _, refusal := range []struct {
		name         string
		change       func(*db.AgentTaskQueue, *db.AgentRuntime, *AgentTaskResponse)
		workspace    string
		capabilities []string
	}{
		{"old daemon", func(*db.AgentTaskQueue, *db.AgentRuntime, *AgentTaskResponse) {}, workspaceID, nil},
		{"changed owner", func(_ *db.AgentTaskQueue, current *db.AgentRuntime, _ *AgentTaskResponse) {
			current.OwnerID = parseUUID(otherID)
		}, workspaceID, []string{taskgateway.Capability}},
		{"missing owner", func(_ *db.AgentTaskQueue, current *db.AgentRuntime, _ *AgentTaskResponse) {
			current.OwnerID.Valid = false
		}, workspaceID, []string{taskgateway.Capability}},
		{"moved task", func(current *db.AgentTaskQueue, _ *db.AgentRuntime, _ *AgentTaskResponse) {
			current.RuntimeID = parseUUID(otherID)
		}, workspaceID, []string{taskgateway.Capability}},
		{"moved runtime", func(current *db.AgentTaskQueue, target *db.AgentRuntime, _ *AgentTaskResponse) {
			current.RuntimeID = parseUUID(otherID)
			target.ID = parseUUID(otherID)
		}, workspaceID, []string{taskgateway.Capability}},
		{"different task", func(current *db.AgentTaskQueue, _ *db.AgentRuntime, _ *AgentTaskResponse) {
			current.ID = parseUUID(otherID)
		}, workspaceID, []string{taskgateway.Capability}},
		{"custom profile", func(_ *db.AgentTaskQueue, current *db.AgentRuntime, _ *AgentTaskResponse) {
			current.ProfileID = parseUUID(otherID)
		}, workspaceID, []string{taskgateway.Capability}},
		{"response workspace", func(_ *db.AgentTaskQueue, _ *db.AgentRuntime, reply *AgentTaskResponse) { reply.WorkspaceID = otherID }, workspaceID, []string{taskgateway.Capability}},
		{"authenticated workspace", func(*db.AgentTaskQueue, *db.AgentRuntime, *AgentTaskResponse) {}, otherID, []string{taskgateway.Capability}},
		{"missing workspace", func(*db.AgentTaskQueue, *db.AgentRuntime, *AgentTaskResponse) {}, "", []string{taskgateway.Capability}},
	} {
		test.Run(refusal.name, func(test *testing.T) {
			current, target, reply := task, runtime, response
			refusal.change(&current, &target, &reply)
			if err := handler.applyTaskGatewayClaimPolicy(current, target, refusal.workspace, &reply, refusal.capabilities); err == nil || reply.RequireCredentialIsolation || reply.CredentialExecutionBinding != nil {
				test.Fatal("refused claim retained an admitted or stale binding")
			}
		})
	}
	if err := handler.applyTaskGatewayClaimPolicy(task, runtime, workspaceID, nil, []string{taskgateway.Capability}); err == nil {
		test.Fatal("missing delivery admitted")
	}
	unmanaged := &Handler{}
	if err := unmanaged.applyTaskGatewayClaimPolicy(task, runtime, workspaceID, &response, nil); err != nil || response.RequireCredentialIsolation || response.CredentialExecutionBinding == nil {
		test.Fatal("default-off changed unmanaged delivery")
	}
}
