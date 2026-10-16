package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/taskgateway"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func TestTaskGatewayExecutionAuthenticatedRefresh(test *testing.T) {
	if _, err := os.Stat("/usr/bin/bwrap"); os.IsNotExist(err) {
		test.Skip("prepared refresh NOT-RUN: bubblewrap unavailable")
	}
	for _, provider := range []string{"claude", "codex"} {
		for _, resume := range []bool{false, true} {
			for _, outcome := range []string{"cancel", "close", "launch-close"} {
				name := provider + "/launch"
				if resume {
					name = provider + "/resume"
				}
				name += "/" + outcome
				test.Run(name, func(test *testing.T) {
					task, binding := ownedTaskGatewayClaim()
					task.Agent.Instructions += " owned readonly input control"
					task.Repos = []RepoData{{URL: "https://example.invalid/owned", Description: "authorized repository"}}
					task.IssueStatuses = []IssueStatusData{{Key: "owned", Name: "authorized status"}}
					task.ProjectResources = []ProjectResourceData{{ID: "owned-resource", ResourceRef: json.RawMessage(`{"url":"https://example.invalid/owned"}`)}}
					if resume {
						task.PriorSessionID = "owned-session"
					}
					root := test.TempDir()
					helper, err := os.Executable()
					if err != nil {
						test.Fatal(err)
					}
					contents, err := os.ReadFile(helper)
					if err != nil {
						test.Fatal(err)
					}
					executable := filepath.Join(root, "task-gateway-native-"+provider)
					if err := os.WriteFile(executable, contents, 0500); err != nil {
						test.Fatal(err)
					}
					var grants, refreshes, providerCalls atomic.Int32
					var hard429 atomic.Bool
					gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						providerCalls.Add(1)
						if request.Header.Get("X-Multica-Task-Id") != binding.TaskID {
							test.Error("refresh changed canonical quota identity")
						}
						if hard429.Load() {
							writer.WriteHeader(http.StatusTooManyRequests)
							return
						}
						writer.Header().Set("Content-Type", "application/json")
						body := ownedDaemonClaudeResponse
						if provider == "codex" {
							body = ownedDaemonOpenAIResponse
							if request.Header.Get("Authorization") != "Bearer owned-task-key" {
								test.Error("refresh changed task credential")
							}
						} else if request.Header.Get("X-Api-Key") != "owned-task-key" {
							test.Error("refresh changed task credential")
						}
						_, _ = writer.Write([]byte(body))
					}))
					defer gateway.Close()
					snapshot := taskgateway.InputSnapshot{Binding: binding, RuntimeID: task.RuntimeID, AgentID: task.AgentID, DispatchedAt: task.DispatchedAt, Instructions: task.Agent.Instructions + " authorized refresh", WorkspaceContext: "authenticated refreshed context"}
					encoded, err := taskgateway.EncodeInputSnapshot(snapshot)
					if err != nil {
						test.Fatal(err)
					}
					var response atomic.Value
					response.Store(encoded)
					started := make(chan struct{}, 1)
					server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						if request.Header.Get("Authorization") != "Bearer "+task.TaskGatewayDaemonToken || request.Header.Get("X-Client-Capabilities") != taskgateway.Capability {
							test.Error("refresh lost authenticated daemon boundary")
						}
						var payload struct {
							TaskToken    string `json:"task_token"`
							DispatchedAt string `json:"dispatched_at"`
						}
						if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.TaskToken != task.AuthToken || payload.DispatchedAt != task.DispatchedAt {
							test.Error("refresh lost committed task claim")
						}
						switch request.URL.Path {
						case "/api/daemon/runtimes/" + task.RuntimeID + "/tasks/" + task.ID + "/gateway-inputs":
							refreshes.Add(1)
							contents := response.Load().([]byte)
							if len(contents) == 0 {
								started <- struct{}{}
								<-request.Context().Done()
								return
							}
							_, _ = writer.Write(contents)
						case "/api/daemon/runtimes/" + task.RuntimeID + "/tasks/" + task.ID + "/gateway-grant":
							grants.Add(1)
							_ = json.NewEncoder(writer).Encode(map[string]any{"binding": binding, "base_url": gateway.URL, "key": "owned-task-key"})
						default:
							test.Error("refresh used an unmanaged route")
							writer.WriteHeader(http.StatusNotFound)
						}
					}))
					defer server.Close()
					client := NewClient(server.URL)
					execution, err := client.prepareTaskGatewayExecution(context.Background(), task, credentialexec.Spec{Root: filepath.Join(root, "private"), Binding: binding, Provider: provider, Executable: executable, HelperExecutable: helper}, agent.ExecOptions{Timeout: 5 * time.Second, HandshakeTimeout: time.Second, ResumeSessionID: task.PriorSessionID})
					if err != nil {
						test.Fatal(err)
					}
					defer execution.Close()
					originalInputs, err := taskGatewayInputs(execution.task, provider)
					if err != nil {
						test.Fatal(err)
					}
					task.Repos[0].Description = "forged repository"
					task.IssueStatuses[0].Name = "forged status"
					task.ProjectResources[0].ID = "forged-resource"
					task.ProjectResources[0].ResourceRef[0] = '!'
					copiedInputs, err := taskGatewayInputs(execution.task, provider)
					if err != nil || !reflect.DeepEqual(originalInputs, copiedInputs) {
						test.Fatal("prepared input metadata aliases caller memory", err)
					}
					originalPrompt := execution.prompt
					for _, invalid := range [][]byte{
						bytes.Replace(encoded, []byte(binding.OwnerID), []byte("00000000-0000-4000-8000-000000000009"), 1),
						bytes.Replace(encoded, []byte(task.RuntimeID), []byte("00000000-0000-4000-8000-000000000009"), 1),
						bytes.Replace(encoded, []byte(task.AgentID), []byte("00000000-0000-4000-8000-000000000009"), 1),
						bytes.Replace(encoded, []byte(task.DispatchedAt), []byte("2026-10-09T17:00:00Z"), 1),
						append(append([]byte(nil), encoded...), []byte(`{}`)...),
						[]byte(`{"instructions":"owned-secret"}`),
					} {
						response.Store(invalid)
						if err := execution.Refresh(context.Background()); err == nil || strings.Contains(err.Error(), "secret") || execution.prompt != originalPrompt || execution.boundary.VerifyInputs(context.Background()) != nil {
							test.Fatal("invalid refresh changed a prepared execution", err)
						}
						if result, err := execution.Run(context.Background(), nil); err == nil || strings.Contains(err.Error(), "secret") || result.Status != "" || providerCalls.Load() != 0 || execution.prompt != originalPrompt {
							test.Fatal("native launch admitted invalid authenticated input", result.Status, err)
						}
					}
					response.Store([]byte{})
					refreshCtx, cancelRefresh := context.WithCancel(context.Background())
					defer cancelRefresh()
					finished := make(chan error, 1)
					go func() {
						if outcome == "launch-close" {
							_, err := execution.Run(refreshCtx, nil)
							finished <- err
							return
						}
						finished <- execution.Refresh(refreshCtx)
					}()
					select {
					case <-started:
					case <-time.After(2 * time.Second):
						test.Fatal("refresh did not enter authenticated request")
					}
					before := refreshes.Load()
					if err := execution.Refresh(context.Background()); err == nil || refreshes.Load() != before {
						test.Fatal("active execution admitted another refresh")
					}
					if _, err := execution.Run(context.Background(), nil); err == nil || providerCalls.Load() != 0 {
						test.Fatal("refresh admitted a concurrent native launch")
					}
					if strings.Contains(outcome, "close") {
						closed := make(chan error, 1)
						go func() { closed <- execution.Close() }()
						select {
						case err := <-closed:
							if err != nil {
								test.Fatal(err)
							}
						case <-time.After(2 * time.Second):
							test.Fatal("close did not cancel and join authenticated refresh")
						}
						if err := <-finished; err == nil || execution.prompt != originalPrompt {
							test.Fatal("close committed cancelled refresh input")
						}
						if err := execution.Refresh(context.Background()); err == nil || refreshes.Load() != before || providerCalls.Load() != 0 {
							test.Fatal("closed execution admitted input refresh")
						}
						return
					}
					cancelRefresh()
					select {
					case err := <-finished:
						if err == nil || execution.prompt != originalPrompt {
							test.Fatal("cancelled refresh committed input")
						}
					case <-time.After(2 * time.Second):
						test.Fatal("cancelled refresh did not join")
					}
					response.Store(encoded)
					if err := execution.Refresh(context.Background()); err != nil {
						test.Fatal(err)
					}
					resolved := execution.task
					resolvedAgent := *task.Agent
					resolved.Agent = &resolvedAgent
					resolved.Agent.Instructions, resolved.WorkspaceContext = snapshot.Instructions, snapshot.WorkspaceContext
					if execution.prompt != BuildPrompt(resolved, provider) || task.Agent.Instructions == snapshot.Instructions || task.WorkspaceContext == snapshot.WorkspaceContext || grants.Load() != 1 || providerCalls.Load() != 0 {
						test.Fatal("refresh mutated the original claim, provisioned or lost authenticated input")
					}
					if contents, err := os.ReadFile(filepath.Join(execution.boundary.WorkDir(), "multica-input", "project", "resources.json")); err != nil || !bytes.Equal(contents, originalInputs["multica-input/project/resources.json"]) {
						test.Fatal("refresh changed pinned project resource metadata", err)
					}
					response.Store([]byte(`{"instructions":"owned-secret"}`))
					promptBefore := execution.prompt
					usageBefore := execution.boundary.UsageSnapshot()
					if result, err := execution.Run(context.Background(), nil); err == nil || result.Status != "" || providerCalls.Load() != 0 || execution.prompt != promptBefore || !reflect.DeepEqual(usageBefore, execution.boundary.UsageSnapshot()) {
						test.Fatal("native launch ignored unavailable authenticated refresh", result.Status, err)
					}
					if _, err := os.Stat(filepath.Join(root, "private", binding.TaskID, "home", "owned-launches")); !os.IsNotExist(err) {
						test.Fatal("refused refresh spawned a native executable", err)
					}
					snapshot.Instructions += " automatic pre-launch refresh"
					encoded, err = taskgateway.EncodeInputSnapshot(snapshot)
					if err != nil {
						test.Fatal(err)
					}
					response.Store(encoded)
					resolved.Agent.Instructions = snapshot.Instructions
					before = refreshes.Load()
					result, err := execution.Run(context.Background(), nil)
					if err != nil || result.Status != "completed" || result.Output != "owned prepared success" || result.SessionID != "owned-session" || providerCalls.Load() != 1 || refreshes.Load() != before+1 || execution.prompt != BuildPrompt(resolved, provider) {
						test.Fatal("refreshed launch/resume failed", result.Status, result.Output, err)
					}
					usage := execution.boundary.UsageSnapshot()
					if err := execution.Refresh(context.Background()); err != nil || !reflect.DeepEqual(usage, execution.boundary.UsageSnapshot()) || grants.Load() != 1 || providerCalls.Load() != 1 {
						test.Fatal("idempotent refresh changed observed usage or provisioned", err)
					}
					hard429.Store(true)
					_, _ = execution.Run(context.Background(), nil)
					before = refreshes.Load()
					if err := execution.Refresh(context.Background()); err == nil || refreshes.Load() != before || grants.Load() != 1 || providerCalls.Load() != 2 {
						test.Fatal("refresh cleared a hard-429 stop or provisioned again")
					}
				})
			}
		}
	}
}
