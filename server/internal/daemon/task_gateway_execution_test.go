package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/taskgateway"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == credentialexec.HelperArg {
		if err := credentialexec.RunHelper(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if strings.HasPrefix(filepath.Base(os.Args[0]), "task-gateway-native-") {
		taskGatewayNativeFixture()
		os.Exit(0)
	}
}

const ownedDaemonClaudeResponse = `{"id":"msg_owned","type":"message","role":"assistant","model":"owned-model","content":[],"stop_reason":"end_turn","usage":{"input_tokens":7,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":5}}`
const ownedDaemonOpenAIResponse = `{"id":"resp_owned","object":"response","model":"owned-model","status":"completed","output":[],"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":17}}`

func taskGatewayNativeFixture() {
	provider := os.Getenv("MULTICA_CREDENTIAL_PROVIDER")
	launches := filepath.Join(os.Getenv("HOME"), "owned-launches")
	previous, _ := os.ReadFile(launches)
	_ = os.WriteFile(launches, append(previous, '1'), 0600)
	invoke := func(prompt string) string {
		staged, err := os.ReadFile("multica-input/prompt.md")
		if err != nil || string(staged) != prompt {
			return "unbound prompt"
		}
		instructions, err := os.ReadFile("multica-input/instructions.md")
		if err != nil {
			return "unbound instructions"
		}
		if strings.Contains(string(instructions), "owned readonly input control") {
			if err := filepath.WalkDir("multica-input", func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() {
					if err := os.WriteFile(filepath.Join(path, "unexpected-input"), []byte("untrusted"), 0600); !errors.Is(err, syscall.EROFS) {
						if err == nil {
							return fmt.Errorf("trusted input directory permits new entries: %s", path)
						}
						return fmt.Errorf("trusted input directory is not read-only: %s: %w", path, err)
					}
					if err := os.Rename(path, path+"-moved"); !errors.Is(err, syscall.EBUSY) && !errors.Is(err, syscall.EROFS) {
						if err == nil {
							return fmt.Errorf("trusted input directory can be renamed: %s", path)
						}
						return fmt.Errorf("trusted input directory is not pinned: %s: %w", path, err)
					}
					return nil
				}
				if err := os.WriteFile(path, []byte("forged native input"), 0600); !errors.Is(err, syscall.EROFS) {
					if err == nil {
						return fmt.Errorf("trusted input is writable: %s", path)
					}
					return fmt.Errorf("trusted input is not read-only: %s: %w", path, err)
				}
				if err := os.Remove(path); !errors.Is(err, syscall.EBUSY) && !errors.Is(err, syscall.EROFS) {
					if err == nil {
						return fmt.Errorf("trusted input was removed: %s", path)
					}
					return fmt.Errorf("trusted input is not pinned: %s: %w", path, err)
				}
				return nil
			}); err != nil {
				return err.Error()
			}
			if err := os.WriteFile("owned-work-state", []byte("retained mutable work"), 0600); err != nil {
				return "task workdir is not writable"
			}
		}
		for _, value := range os.Environ() {
			if strings.Contains(value, "mdt_owned") || strings.Contains(value, "mat_owned") || strings.Contains(value, "owned-task-key") || strings.Contains(value, "owned-unlimited") {
				return "credential leaked"
			}
		}
		endpoint := os.Getenv("ANTHROPIC_BASE_URL") + "/v1/messages"
		if provider == "codex" {
			endpoint = os.Getenv("OPENAI_BASE_URL") + "/responses"
		}
		client := &http.Client{Timeout: time.Second}
		defer client.CloseIdleConnections()
		response, err := client.Post(endpoint, "application/json", strings.NewReader(`{"model":"owned-model"}`))
		if err != nil {
			return "gateway unavailable"
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || response.StatusCode != 200 {
			return "forged success"
		}
		return "owned prepared success"
	}
	decoder := json.NewDecoder(os.Stdin)
	if provider == "claude" {
		var message struct {
			Message struct{ Content []struct{ Text string } }
		}
		if decoder.Decode(&message) != nil {
			return
		}
		if len(message.Message.Content) != 1 {
			return
		}
		result := invoke(message.Message.Content[0].Text)
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "subtype": "success", "result": result, "session_id": "owned-session"})
		return
	}
	for {
		var request struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		if decoder.Decode(&request) != nil {
			return
		}
		switch request.Method {
		case "initialize":
			fmt.Printf("{\"id\":%s,\"result\":{}}\n", request.ID)
		case "thread/start", "thread/resume":
			fmt.Printf("{\"id\":%s,\"result\":{\"thread\":{\"id\":\"owned-session\"}}}\n", request.ID)
		case "turn/start":
			var params struct{ Input []struct{ Text string } }
			_ = json.Unmarshal(request.Params, &params)
			prompt := ""
			if len(params.Input) != 0 {
				prompt = params.Input[0].Text
			}
			fmt.Printf("{\"id\":%s,\"result\":{}}\n", request.ID)
			result := invoke(prompt)
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "owned-session", "item": map[string]any{"type": "agentMessage", "id": "owned-message", "text": result}}})
			fmt.Println(`{"method":"turn/completed","params":{"threadId":"owned-session","turn":{"id":"owned-turn","status":"completed"}}}`)
		}
	}
}

func ownedTaskGatewayClaim() (Task, credentialexec.Binding) {
	binding := credentialexec.Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"}
	task := Task{ID: binding.TaskID, WorkspaceID: binding.WorkspaceID, RuntimeID: "00000000-0000-4000-8000-000000000004", AgentID: "00000000-0000-4000-8000-000000000005", RequireCredentialIsolation: true, CredentialExecutionBinding: &binding, TaskGatewayDaemonToken: "mdt_owned-daemon-secret", AuthToken: "mat_owned-task-secret", DispatchedAt: "2026-10-08T12:00:00Z"}
	task.Agent = &AgentData{ID: task.AgentID, Instructions: "owned bound instructions", Model: "owned-model"}
	return task, binding
}

func TestTaskGatewayExecutionPreparedLaunchAndResume(test *testing.T) {
	if _, err := os.Stat("/usr/bin/bwrap"); os.IsNotExist(err) {
		test.Skip("prepared daemon launch NOT-RUN: bubblewrap unavailable")
	}
	for _, provider := range []string{"claude", "codex"} {
		test.Run(provider, func(test *testing.T) {
			task, binding := ownedTaskGatewayClaim()
			task.Agent.Instructions += " owned readonly input control"
			bundle := makeResolvableSkillBundle("owned-skill")
			task.Agent.SkillRefs = []SkillRefData{skillRefFromBundle(bundle)}
			task.ChatSessionID = "00000000-0000-4000-8000-000000000009"
			task.ChatMessage = "Use [/owned-skill](slash://skill/owned-skill)."
			resolved := task
			resolvedAgent := *task.Agent
			resolved.Agent = &resolvedAgent
			resolved.Agent.SkillRefs, resolved.Agent.Skills = nil, []SkillData{bundle}
			if BuildPrompt(resolved, provider) == BuildPrompt(task, provider) {
				test.Fatal("fixture did not distinguish resolved native and staged prompts")
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
			spec := credentialexec.Spec{Root: filepath.Join(root, "private"), Binding: binding, Provider: provider, Executable: executable, HelperExecutable: helper}
			var grants, calls atomic.Int32
			var refuse atomic.Bool
			gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if request.Header.Get("X-Multica-Task-Id") != binding.TaskID || provider == "claude" && request.Header.Get("X-Api-Key") != "owned-task-key" || provider == "codex" && request.Header.Get("Authorization") != "Bearer owned-task-key" {
					test.Error("authoritative gateway binding lost")
				}
				if refuse.Load() {
					writer.WriteHeader(http.StatusTooManyRequests)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				body := ownedDaemonClaudeResponse
				if provider == "codex" {
					body = ownedDaemonOpenAIResponse
				}
				_, _ = io.WriteString(writer, body)
			}))
			defer gateway.Close()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer "+task.TaskGatewayDaemonToken {
					test.Error("owning-daemon authentication lost")
				}
				if strings.HasSuffix(request.URL.Path, "/skill-bundles/resolve") {
					_ = json.NewEncoder(writer).Encode(map[string]any{"bundles": []SkillData{bundle}})
					return
				}
				grants.Add(1)
				if contents, err := os.ReadFile(filepath.Join(spec.Root, task.ID, "workdir", "multica-input", "prompt.md")); err != nil || string(contents) != BuildPrompt(resolved, provider) {
					test.Error("grant precedes preparation/input staging")
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"binding": binding, "base_url": gateway.URL, "key": "owned-task-key"})
			}))
			defer server.Close()
			client := NewClient(server.URL)
			for attempt := 1; attempt <= 2; attempt++ {
				options := agent.ExecOptions{Timeout: 5 * time.Second, HandshakeTimeout: time.Second}
				if attempt == 2 {
					task.PriorSessionID = "owned-session"
					options.ResumeSessionID = "owned-session"
				}
				execution, err := client.prepareTaskGatewayExecution(context.Background(), task, spec, options)
				if err != nil {
					test.Fatal(err)
				}
				test.Cleanup(func() { _ = execution.Close() })
				promptPath := filepath.Join(execution.boundary.WorkDir(), "multica-input", "prompt.md")
				for _, mutation := range []string{"changed", "missing"} {
					var err error
					if mutation == "changed" {
						err = os.WriteFile(promptPath, []byte("forged native input"), 0600)
					} else {
						err = os.Remove(promptPath)
					}
					if err != nil {
						test.Fatal(err)
					}
					if result, err := execution.Run(context.Background(), nil); err == nil || result.Status != "" || calls.Load() != int32(attempt-1) {
						test.Fatal("tampered or missing input launched", result, err)
					}
					if mutation == "missing" {
						if _, err := os.Lstat(promptPath); !os.IsNotExist(err) {
							test.Fatal("launch verification repaired missing input", err)
						}
					}
					if err := os.WriteFile(promptPath, []byte(BuildPrompt(resolved, provider)), 0600); err != nil {
						test.Fatal(err)
					}
				}
				for _, relative := range []string{"multica-input/unexpected.md", "multica-input/skills/owned-skill/unexpected.md"} {
					path := filepath.Join(execution.boundary.WorkDir(), relative)
					if err := os.WriteFile(path, []byte("untrusted extra input"), 0600); err != nil {
						test.Fatal(err)
					}
					if result, err := execution.Run(context.Background(), nil); err == nil || result.Status != "" || calls.Load() != int32(attempt-1) {
						test.Fatal("unexpected input launched", result, err)
					}
					if err := os.Remove(path); err != nil {
						test.Fatal(err)
					}
				}
				result, err := execution.Run(context.Background(), nil)
				if err != nil || result.Status != "completed" || result.Output != "owned prepared success" || result.SessionID != "owned-session" {
					test.Fatalf("bound launch/resume: %+v err=%v", result, err)
				}
				if err := execution.boundary.VerifyInputs(context.Background()); err != nil {
					test.Fatal("native launch/resume changed the authorized snapshot", err)
				}
				if contents, err := os.ReadFile(filepath.Join(execution.boundary.WorkDir(), "owned-work-state")); err != nil || string(contents) != "retained mutable work" {
					test.Fatal("native launch/resume lost mutable task work", err)
				}
				if result.GatewayUsage == nil || !result.GatewayUsage.Complete || len(result.GatewayUsage.Models) != 1 {
					test.Fatal("prepared execution lost observed gateway usage", result.GatewayUsage)
				}
				started := make(chan struct{})
				execution.backend = ownedPreparedBackend(func(ctx context.Context, _ string, _ agent.ExecOptions) (*agent.Session, error) {
					results := make(chan agent.Result, 1)
					close(started)
					go func() {
						<-ctx.Done()
						results <- agent.Result{Status: "canceled"}
						close(results)
					}()
					return &agent.Session{Result: results}, nil
				})
				finished := make(chan error, 1)
				go func() {
					result, err := execution.Run(context.Background(), nil)
					if err == nil && result.Status != "canceled" {
						err = fmt.Errorf("cancel lost terminal result: %s", result.Status)
					}
					finished <- err
				}()
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					test.Fatal("prepared execution did not start")
				}
				if _, err := execution.Run(context.Background(), nil); err == nil {
					test.Fatal("concurrent prepared execution admitted")
				}
				if err := execution.Close(); err != nil {
					test.Fatal(err)
				}
				if err := <-finished; err != nil {
					test.Fatal(err)
				}
				if err := execution.Close(); err != nil {
					test.Fatal("repeated close failed", err)
				}
				if launches, err := os.ReadFile(filepath.Join(spec.Root, task.ID, "home", "owned-launches")); err != nil || string(launches) != strings.Repeat("1", attempt) {
					test.Fatal("same-task native state reset", string(launches), err)
				}
				if _, err := execution.Run(context.Background(), nil); err == nil {
					test.Fatal("closed execution launched")
				}
			}
			refuse.Store(true)
			execution, err := client.prepareTaskGatewayExecution(context.Background(), task, spec, agent.ExecOptions{Timeout: 5 * time.Second, HandshakeTimeout: time.Second, ResumeSessionID: task.PriorSessionID})
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() { _ = execution.Close() })
			result, err := execution.Run(context.Background(), nil)
			if err != nil || result.Status != "failed" || result.GatewayUsage == nil || result.GatewayUsage.Complete {
				test.Fatal("prepared quota refusal reported success", result, err)
			}
			if _, err := execution.Run(context.Background(), nil); err == nil {
				test.Fatal("quota refusal allowed another native launch")
			}
			if err := execution.Close(); err != nil {
				test.Fatal(err)
			}
			if repeated, err := client.prepareTaskGatewayExecution(context.Background(), task, spec, agent.ExecOptions{}); err == nil || repeated != nil {
				test.Fatal("stopped task requested another grant")
			}
			if grants.Load() != 3 || calls.Load() != 3 {
				test.Fatal("launch/resume repeated provisioning/request", grants.Load(), calls.Load())
			}
		})
	}
}

func TestTaskGatewayExecutionRefusesBeforeGrant(test *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { calls.Add(1); writer.WriteHeader(500) }))
	defer server.Close()
	client := NewClient(server.URL)
	task, binding := ownedTaskGatewayClaim()
	root := test.TempDir()
	helper, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	contents, err := os.ReadFile(helper)
	if err != nil {
		test.Fatal(err)
	}
	executable := filepath.Join(root, "task-gateway-native-codex")
	if err := os.WriteFile(executable, contents, 0500); err != nil {
		test.Fatal(err)
	}
	spec := credentialexec.Spec{Root: filepath.Join(root, "private"), Binding: binding, Provider: "codex", Executable: executable, HelperExecutable: helper}
	for _, mutate := range []func(*Task, *agent.ExecOptions){
		func(task *Task, _ *agent.ExecOptions) { task.Agent.CustomArgs = []string{"--profile", "unlimited"} },
		func(task *Task, _ *agent.ExecOptions) { task.Agent.McpConfig = json.RawMessage(`{"servers":{}}`) },
		func(task *Task, _ *agent.ExecOptions) {
			task.Agent.RuntimeConfig = json.RawMessage(`{"profile":"unlimited"}`)
		},
		func(task *Task, _ *agent.ExecOptions) {
			task.Agent.CustomEnv = map[string]string{"OPENAI_API_KEY": "owned-unlimited"}
		},
		func(_ *Task, options *agent.ExecOptions) { options.ExtraArgs = []string{"--config", "unlimited"} },
		func(_ *Task, options *agent.ExecOptions) { options.Cwd = "/owned/unmanaged" },
		func(_ *Task, options *agent.ExecOptions) { options.Model = "forged-model" },
		func(_ *Task, options *agent.ExecOptions) { options.SystemPrompt = "forged" },
		func(_ *Task, options *agent.ExecOptions) { options.EnableTaskSupplement = true },
		func(_ *Task, options *agent.ExecOptions) { options.CodexSQLiteInitRetry = true },
		func(_ *Task, options *agent.ExecOptions) { options.ResumeSessionID = "forged-session" },
		func(task *Task, _ *agent.ExecOptions) { task.PriorSessionID = "claimed-session" },
		func(task *Task, _ *agent.ExecOptions) { task.PriorSessionResumeUnavailable = true },
	} {
		candidate := task
		agentData := *task.Agent
		candidate.Agent = &agentData
		options := agent.ExecOptions{}
		mutate(&candidate, &options)
		if execution, err := client.prepareTaskGatewayExecution(context.Background(), candidate, spec, options); err == nil || execution != nil || calls.Load() != 0 {
			test.Fatal("unsupported launch prepared/provisioned")
		}
	}
	test.Run("prepared-boundary", func(test *testing.T) {
		if _, err := os.Stat("/usr/bin/bwrap"); os.IsNotExist(err) {
			test.Skip("prepared daemon grant fixture NOT-RUN: bubblewrap unavailable")
		}
		if execution, err := client.prepareTaskGatewayExecution(context.Background(), task, spec, agent.ExecOptions{}); err == nil || execution != nil || calls.Load() != 1 {
			test.Fatal("valid OS preparation did not reach refused grant", calls.Load(), err)
		}
		spec.Executable = "/owned/missing"
		if execution, err := client.prepareTaskGatewayExecution(context.Background(), task, spec, agent.ExecOptions{}); err == nil || execution != nil || calls.Load() != 1 {
			test.Fatal("missing OS boundary requested credentials")
		}
	})
	if taskgateway.Capability == "" {
		test.Fatal("missing capability contract")
	}
}

type ownedPreparedBackend func(context.Context, string, agent.ExecOptions) (*agent.Session, error)

func (backend ownedPreparedBackend) Execute(ctx context.Context, prompt string, options agent.ExecOptions) (*agent.Session, error) {
	return backend(ctx, prompt, options)
}
