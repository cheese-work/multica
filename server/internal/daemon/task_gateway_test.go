package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/taskgateway"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func TestTaskGatewayDaemonPreparedDelivery(test *testing.T) {
	binding := credentialexec.Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"}
	root := test.TempDir()
	helper, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	contents, err := os.ReadFile(helper)
	if err != nil {
		test.Fatal(err)
	}
	executable := filepath.Join(root, "owned-native")
	if err := os.WriteFile(executable, contents, 0500); err != nil {
		test.Fatal(err)
	}
	spec := credentialexec.Spec{Root: filepath.Join(root, "private"), Binding: binding, Provider: "codex", Executable: executable, HelperExecutable: helper}
	task := Task{ID: binding.TaskID, WorkspaceID: binding.WorkspaceID, RuntimeID: "00000000-0000-4000-8000-000000000004", RequireCredentialIsolation: true, CredentialExecutionBinding: &binding, TaskGatewayDaemonToken: "mdt_owned-daemon-secret", AuthToken: "mat_owned-task-secret", DispatchedAt: "2026-10-07T19:00:00Z"}
	var calls atomic.Int32
	var status atomic.Int32
	status.Store(http.StatusOK)
	var response atomic.Value
	response.Store(`{"binding":{"task_id":"` + binding.TaskID + `","owner_id":"` + binding.OwnerID + `","workspace_id":"` + binding.WorkspaceID + `"},"base_url":"https://owned.example","key":"owned-gateway-secret"}`)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if _, err := os.Stat(filepath.Join(spec.Root, binding.TaskID, "binding.json")); err != nil {
			test.Error("grant requested before OS preparation")
		}
		if request.Header.Get("Authorization") != "Bearer "+task.TaskGatewayDaemonToken || request.Header.Get("X-Client-Capabilities") != taskgateway.Capability || request.Method != http.MethodPost || request.URL.Path != "/api/daemon/runtimes/"+task.RuntimeID+"/tasks/"+task.ID+"/gateway-grant" {
			test.Error("wrong authenticated daemon route")
		}
		var body map[string]string
		if json.NewDecoder(request.Body).Decode(&body) != nil || len(body) != 2 || body["task_token"] != task.AuthToken || body["dispatched_at"] != task.DispatchedAt {
			test.Error("wrong claim binding")
		}
		writer.Header().Set("Location", "/unlimited-fallback")
		writer.WriteHeader(int(status.Load()))
		_, _ = writer.Write([]byte(response.Load().(string)))
	}))
	defer server.Close()
	client := NewClient(server.URL)
	missing := spec
	missing.Root = filepath.Join(root, "failed-private")
	missing.Executable = "/owned/missing-native"
	if boundary, err := client.PrepareTaskGateway(context.Background(), task, missing); err == nil || boundary != nil || calls.Load() != 0 {
		test.Fatal("failed preparation requested credentials")
	}
	for attempt := 0; attempt < 2; attempt++ {
		boundary, err := client.PrepareTaskGateway(context.Background(), task, spec)
		if err != nil {
			test.Fatal(err)
		}
		if attempt == 0 {
			if err := os.WriteFile(filepath.Join(boundary.Home(), "native-state"), []byte("retained"), 0600); err != nil {
				test.Fatal(err)
			}
		} else if contents, err := os.ReadFile(filepath.Join(boundary.Home(), "native-state")); err != nil || string(contents) != "retained" {
			test.Fatal("same-task state lost")
		}
		if err := boundary.Validate(binding.TaskID, "codex", executable); err != nil {
			test.Fatal(err)
		}
		if err := boundary.Close(); err != nil {
			test.Fatal(err)
		}
	}
	for _, code := range []int{http.StatusTooManyRequests, http.StatusFound, http.StatusServiceUnavailable} {
		status.Store(int32(code))
		before := calls.Load()
		if boundary, err := client.PrepareTaskGateway(context.Background(), task, spec); err == nil || boundary != nil || strings.Contains(err.Error(), "owned-gateway-secret") || calls.Load() != before+1 {
			test.Fatal("refused grant retried, leaked or fell back")
		}
	}
	status.Store(http.StatusOK)
	response.Store(strings.Repeat(" ", taskgateway.HandoffLimit+1))
	if boundary, err := client.PrepareTaskGateway(context.Background(), task, spec); err == nil || boundary != nil {
		test.Fatal("oversized grant admitted")
	}
	before := calls.Load()
	task.TaskGatewayDaemonToken = "mul_owner-pat"
	if boundary, err := client.PrepareTaskGateway(context.Background(), task, spec); err == nil || boundary != nil || calls.Load() != before {
		test.Fatal("owner PAT fallback admitted")
	}
}
