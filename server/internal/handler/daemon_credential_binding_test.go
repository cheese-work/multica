package handler

import (
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCredentialExecutionBindingUsesClaimedTaskAndRuntimeOwner(test *testing.T) {
	workspaceID := "00000000-0000-4000-8000-000000000002"
	task := db.AgentTaskQueue{ID: parseUUID("00000000-0000-4000-8000-000000000001"), OriginatorUserID: parseUUID("00000000-0000-4000-8000-000000000004")}
	runtime := db.AgentRuntime{OwnerID: parseUUID("00000000-0000-4000-8000-000000000003")}
	binding := credentialExecutionBindingForClaim(task, runtime, workspaceID)
	if binding == nil || binding.TaskID != uuidToString(task.ID) || binding.OwnerID != uuidToString(runtime.OwnerID) || binding.WorkspaceID != workspaceID {
		test.Fatalf("incorrect authenticated binding: %+v", binding)
	}
	runtime.OwnerID.Valid = false
	if credentialExecutionBindingForClaim(task, runtime, workspaceID) != nil {
		test.Fatal("ownerless runtime admitted")
	}
	task.ID.Valid = false
	if credentialExecutionBindingForClaim(task, db.AgentRuntime{OwnerID: parseUUID("00000000-0000-4000-8000-000000000003")}, workspaceID) != nil {
		test.Fatal("missing task identity admitted")
	}
}
