package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func TestCredentialExclusiveDaemonRefusesBeforePreparation(test *testing.T) {
	task := Task{ID: "00000000-0000-4000-8000-000000000001", WorkspaceID: "00000000-0000-4000-8000-000000000002", RequireCredentialIsolation: true}
	daemon := &Daemon{}
	if _, err := daemon.runTask(context.Background(), task, "codex", 0, nil); err == nil || !strings.Contains(err.Error(), "authenticated") {
		test.Fatalf("missing binding: %v", err)
	}
	task.CredentialExecutionBinding = &credentialexec.Binding{TaskID: task.ID, WorkspaceID: task.WorkspaceID, OwnerID: "00000000-0000-4000-8000-000000000003"}
	if _, err := daemon.runTask(context.Background(), task, "claude", 0, nil); err == nil || !strings.Contains(err.Error(), "not integrated") {
		test.Fatalf("unintegrated path: %v", err)
	}
	task.CredentialExecutionBinding.TaskID = "00000000-0000-4000-8000-000000000004"
	if _, err := daemon.runTask(context.Background(), task, "claude", 0, nil); err == nil || !strings.Contains(err.Error(), "authenticated") {
		test.Fatalf("forged binding: %v", err)
	}
}
