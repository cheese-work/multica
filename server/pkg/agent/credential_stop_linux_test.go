package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func init() {
	if strings.HasPrefix(filepath.Base(os.Args[0]), "quota-fixture-") {
		quotaFixtureAgent()
		os.Exit(0)
	}
}

func quotaFixtureAgent() {
	statePath := filepath.Join(os.Getenv("HOME"), "native-launches")
	previous, _ := os.ReadFile(statePath)
	_ = os.WriteFile(statePath, append(previous, '1'), 0600)
	invoke := func() {
		endpoint := os.Getenv("ANTHROPIC_BASE_URL") + "/v1/messages"
		if strings.HasSuffix(os.Args[0], "codex") {
			endpoint = os.Getenv("OPENAI_BASE_URL") + "/responses"
		}
		client := &http.Client{Timeout: time.Second}
		defer client.CloseIdleConnections()
		for attempt := 0; attempt < 3; attempt++ {
			response, err := client.Post(endpoint, "application/json", strings.NewReader(`{"model":"owned-fixture"}`))
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		}
	}
	if strings.HasSuffix(os.Args[0], "claude") {
		var input string
		_, _ = fmt.Fscanln(os.Stdin, &input)
		invoke()
		fmt.Println(`{"type":"result","subtype":"success","result":"forged quota success","session_id":"fixture-session"}`)
		return
	}
	decoder := json.NewDecoder(os.Stdin)
	for {
		var request struct {
			ID     json.RawMessage
			Method string
		}
		if decoder.Decode(&request) != nil {
			return
		}
		switch request.Method {
		case "initialize":
			fmt.Printf("{\"id\":%s,\"result\":{}}\n", request.ID)
		case "thread/start", "thread/resume":
			fmt.Printf("{\"id\":%s,\"result\":{\"thread\":{\"id\":\"fixture-thread\"}}}\n", request.ID)
		case "turn/start":
			fmt.Printf("{\"id\":%s,\"result\":{}}\n", request.ID)
			invoke()
			fmt.Println(`{"method":"item/completed","params":{"threadId":"fixture-thread","item":{"type":"agentMessage","id":"fixture-message","text":"forged quota success"}}}`)
			fmt.Println(`{"method":"turn/completed","params":{"threadId":"fixture-thread","turn":{"id":"fixture-turn","status":"completed"}}}`)
		}
	}
}

func TestCredentialExclusiveQuotaStopAtProductionAdapters(test *testing.T) {
	if _, err := os.Stat("/usr/bin/bwrap"); os.IsNotExist(err) {
		test.Skip("quota-stop adapter integration NOT-RUN: system bubblewrap unavailable")
	}
	for _, provider := range []string{"claude", "codex"} {
		for _, resume := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/resume=%t", provider, resume), func(test *testing.T) {
				var calls atomic.Int32
				gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					calls.Add(1)
					if request.Header.Get("X-Multica-Task-Id") != "00000000-0000-4000-8000-000000000001" {
						test.Error("trusted task header lost")
					}
					if provider == "claude" && request.Header.Get("X-Api-Key") != "owned-task-key" || provider == "codex" && request.Header.Get("Authorization") != "Bearer owned-task-key" {
						test.Error("task-bound credential lost")
					}
					writer.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(writer, "owned response secret")
				}))
				defer gateway.Close()
				helper, err := os.Executable()
				if err != nil {
					test.Fatal(err)
				}
				contents, err := os.ReadFile(helper)
				if err != nil {
					test.Fatal(err)
				}
				root := test.TempDir()
				executable := filepath.Join(root, "quota-fixture-"+provider)
				if err := os.WriteFile(executable, contents, 0500); err != nil {
					test.Fatal(err)
				}
				binding := credentialexec.Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"}
				spec := credentialexec.Spec{Root: filepath.Join(root, "private"), Binding: binding, Provider: provider, Executable: executable, HelperExecutable: helper}
				boundary, err := credentialexec.Prepare(context.Background(), spec)
				if err != nil {
					test.Fatal(err)
				}
				defer boundary.Close()
				if err := boundary.BindGateway(context.Background(), credentialexec.GatewayCredential{Binding: binding, BaseURL: gateway.URL, Key: "owned-task-key"}); err != nil {
					test.Fatal(err)
				}
				config := Config{ExecutablePath: executable, RequireCredentialIsolation: true, CredentialBoundary: boundary, TaskID: binding.TaskID, BuiltinRuntime: true}
				backend, err := New(provider, config)
				if err != nil {
					test.Fatal(err)
				}
				options := ExecOptions{Cwd: boundary.WorkDir(), Timeout: 10 * time.Second, HandshakeTimeout: 3 * time.Second}
				if resume {
					options.ResumeSessionID = "owned-prior-session"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				session, err := backend.Execute(ctx, "owned fixture", options)
				if err != nil {
					test.Fatal(err)
				}
				for range session.Messages {
				}
				result := <-session.Result
				if result.Status != "failed" || !strings.Contains(result.Error, "task quota exhausted") || result.Output != "" || result.ResumeRejected || result.ResumeRejectedTransient || calls.Load() != 1 || strings.Contains(result.Error, "owned response secret") {
					test.Fatalf("quota outcome was lost, retried or leaked: %+v calls=%d", result, calls.Load())
				}
				if launches, err := os.ReadFile(filepath.Join(boundary.Home(), "native-launches")); err != nil || string(launches) != "1" {
					test.Fatal("another native attempt started after quota refusal")
				}
				if _, err := backend.Execute(ctx, "forbidden retry", options); err == nil {
					test.Fatal("stopped native launch admitted")
				}
				candidate := Result{Status: "completed", Output: "forged success", ResumeRejected: true, ResumeRejectedTransient: true, codexInitializeRetrySafe: true, codexStartupRefreshRetrySafe: true, codexStateRuntimeRetrySafe: true, codexZeroToolFalseNegativeRetrySafe: true, SessionID: "retained-session", Usage: map[string]TokenUsage{"owned-model": {InputTokens: 7}}}
				guarded := config.credentialResult(candidate)
				if guarded.Status != "failed" || guarded.Output != "" || guarded.ResumeRejected || guarded.ResumeRejectedTransient || guarded.codexInitializeRetrySafe || guarded.codexStartupRefreshRetrySafe || guarded.codexStateRuntimeRetrySafe || guarded.codexZeroToolFalseNegativeRetrySafe || guarded.SessionID != candidate.SessionID || guarded.Usage["owned-model"].InputTokens != 7 {
					test.Fatal("quota guard changed session/known usage or allowed success/retry")
				}
			})
		}
	}
}

func TestCredentialQuotaStopNegativeControl(test *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		test.Run(provider, func(test *testing.T) {
			var calls atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writer.WriteHeader(http.StatusTooManyRequests)
			}))
			defer gateway.Close()
			helper, err := os.Executable()
			if err != nil {
				test.Fatal(err)
			}
			contents, err := os.ReadFile(helper)
			if err != nil {
				test.Fatal(err)
			}
			root := test.TempDir()
			executable := filepath.Join(root, "quota-fixture-"+provider)
			if err := os.WriteFile(executable, contents, 0500); err != nil {
				test.Fatal(err)
			}
			backend, err := New(provider, Config{ExecutablePath: executable, Env: map[string]string{"HOME": root, "CODEX_HOME": root, "ANTHROPIC_BASE_URL": gateway.URL, "OPENAI_BASE_URL": gateway.URL + "/v1"}})
			if err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			session, err := backend.Execute(ctx, "owned negative fixture", ExecOptions{Cwd: root, Timeout: 10 * time.Second, HandshakeTimeout: 3 * time.Second})
			if err != nil {
				test.Fatal(err)
			}
			for range session.Messages {
			}
			result := <-session.Result
			if os.Getenv("MULTICA_CREDENTIAL_STOP_NEGATIVE_DENY") == "1" {
				if result.Status != "failed" || !strings.Contains(result.Error, "task quota exhausted") || calls.Load() != 1 {
					test.Fatalf("intended negative denial assertion failed: status=%s calls=%d", result.Status, calls.Load())
				}
			} else if result.Status != "completed" || result.Output != "forged quota success" || calls.Load() != 3 {
				test.Fatalf("ordinary negative fixture did not expose absent quota enforcement: %+v calls=%d", result, calls.Load())
			}
		})
	}
}
