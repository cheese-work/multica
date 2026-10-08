package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == "__owned_credential_peer" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == credentialexec.HelperArg {
		if err := credentialexec.RunHelper(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if strings.HasPrefix(filepath.Base(os.Args[0]), "credential-fixture-") {
		credentialFixtureAgent()
		os.Exit(0)
	}
}

type credentialProbe struct {
	Outside         string
	OtherTask       string
	PeerEnvironment string
	PeerDescriptor  string
	HostURL         string
	PeerSocket      string
	AbstractSocket  string
}

type credentialObservation struct {
	Readable       map[string]bool
	Inherited      bool
	Scoped         bool
	Gateway        bool
	Count          int
	Helper         bool
	HostNetwork    bool
	UnixPeer       bool
	AbstractPeer   bool
	ParentLeak     bool
	DescriptorLeak bool
	NativeControl  bool
	AdminDenied    bool
	StagedInputs   bool
}

func credentialFixtureAgent() {
	if len(os.Args) > 1 && os.Args[1] == "__owned_descriptor_helper" {
		entries, _ := os.ReadDir("/proc/self/fd")
		for _, entry := range entries {
			if entry.Name() == "0" || entry.Name() == "1" || entry.Name() == "2" {
				continue
			}
			for _, suffix := range []string{"", "/../../../unlimited", "/../../unlimited", "/../unlimited"} {
				path := "/proc/self/fd/" + entry.Name() + suffix
				info, err := os.Stat(path)
				if err != nil || !info.Mode().IsRegular() {
					continue
				}
				data, _ := os.ReadFile(path)
				if strings.Contains(string(data), "owned-unlimited") {
					fmt.Print("owned-unlimited")
					return
				}
			}
		}
		return
	}
	var probe credentialProbe
	data, _ := os.ReadFile("probe.json")
	_ = json.Unmarshal(data, &probe)
	observation := credentialObservation{Readable: make(map[string]bool)}
	stagedPrompt, promptErr := os.ReadFile("multica-input/prompt.md")
	stagedSkill, skillErr := os.ReadFile("multica-input/skills/owned/SKILL.md")
	observation.StagedInputs = promptErr == nil && skillErr == nil && string(stagedPrompt) == "owned authenticated prompt" && string(stagedSkill) == "owned authenticated skill"
	for name, path := range map[string]string{"direct": probe.Outside, "symlink": "escape", "cross-task": probe.OtherTask, "peer-env": probe.PeerEnvironment, "peer-fd": probe.PeerDescriptor} {
		data, err := os.ReadFile(path)
		observation.Readable[name] = err == nil && strings.Contains(string(data), "owned-unlimited")
	}
	helper := exec.Command("/bin/sh", "-c", `read credential < "$1"; printf '%s' "$credential"`, "owned-helper", probe.Outside)
	helper.Env = os.Environ()
	helperOutput, _ := helper.Output()
	observation.Helper = strings.Contains(string(helperOutput), "owned-unlimited")
	descriptorHelper := exec.Command(os.Args[0], "__owned_descriptor_helper")
	descriptorHelper.Env = os.Environ()
	descriptorOutput, _ := descriptorHelper.Output()
	observation.DescriptorLeak = strings.Contains(string(descriptorOutput), "owned-unlimited")
	arguments := strings.Join(os.Args[1:], " ")
	if os.Getenv("MULTICA_CREDENTIAL_PROVIDER") == "claude" {
		observation.NativeControl = strings.Contains(arguments, "--permission-mode default") && !strings.Contains(arguments, "bypassPermissions")
	}
	if os.Getenv("MULTICA_CREDENTIAL_PROVIDER") == "codex" {
		observation.NativeControl = strings.Contains(arguments, `sandbox_mode="workspace-write"`) && strings.Contains(arguments, `approval_policy="on-request"`) && !strings.Contains(arguments, "danger-full-access")
	}
	if os.Getenv("MULTICA_CREDENTIAL_PROVIDER") != "" {
		parent, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", os.Getppid()))
		observation.ParentLeak = strings.Contains(string(parent), "owned-unlimited")
	}
	if probe.HostURL != "" {
		client := &http.Client{Timeout: 100 * time.Millisecond}
		response, err := client.Get(probe.HostURL)
		if err == nil {
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			observation.HostNetwork = string(body) == "owned-unlimited"
		}
	}
	for _, socket := range []string{probe.PeerSocket, probe.AbstractSocket} {
		if socket == "" {
			continue
		}
		client := &http.Client{Timeout: 100 * time.Millisecond, Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}}
		response, err := client.Get("http://owned-peer/unlimited")
		if err == nil {
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if strings.HasPrefix(socket, "@") {
				observation.AbstractPeer = string(body) == "owned-unlimited"
			} else {
				observation.UnixPeer = string(body) == "owned-unlimited"
			}
		}
		client.CloseIdleConnections()
	}
	observation.Inherited = os.Getenv("OWNED_UNLIMITED") != "" || os.Getenv("AWS_SECRET_ACCESS_KEY") != "" || os.Getenv("OPENAI_API_KEY") == "owned-unlimited"
	observation.Scoped = os.Getenv("OPENAI_API_KEY") == credentialexec.NativePlaceholder || os.Getenv("ANTHROPIC_API_KEY") == credentialexec.NativePlaceholder
	statePath := filepath.Join(os.Getenv("HOME"), "continuity")
	state, _ := os.ReadFile(statePath)
	_, _ = fmt.Sscanf(string(state), "%d", &observation.Count)
	observation.Count++
	_ = os.WriteFile(statePath, []byte(fmt.Sprint(observation.Count)), 0600)
	if base := os.Getenv("ANTHROPIC_BASE_URL"); observation.Scoped && base != "" {
		endpoint := "/v1/messages"
		if os.Getenv("MULTICA_CREDENTIAL_PROVIDER") == "codex" {
			endpoint = "/v1/responses"
		}
		request, _ := http.NewRequest(http.MethodPost, base+endpoint, strings.NewReader(`{"model":"owned-fixture"}`))
		request.Header.Set("X-Multica-Task-Id", "forged")
		request.Header.Set("X-Api-Key", "forged")
		client := &http.Client{Timeout: time.Second}
		response, err := client.Do(request)
		if err == nil {
			payload, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			observation.Gateway = response.StatusCode == http.StatusOK && string(payload) == "owned-scoped"
		}
		adminResponse, err := client.Get(base + "/admin/credentials")
		if err == nil {
			observation.AdminDenied = adminResponse.StatusCode == http.StatusForbidden
			_ = adminResponse.Body.Close()
		}
		client.CloseIdleConnections()
	}
	payload, _ := json.Marshal(observation)
	if strings.HasSuffix(os.Args[0], "claude") {
		var input string
		_, _ = fmt.Fscanln(os.Stdin, &input)
		result, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "result": string(payload), "session_id": "fixture-session"})
		fmt.Println(string(result))
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
		if request.Method == "initialize" {
			fmt.Printf("{\"id\":%s,\"result\":{}}\n", request.ID)
		} else if request.Method == "thread/start" || request.Method == "thread/resume" {
			fmt.Printf("{\"id\":%s,\"result\":{\"thread\":{\"id\":\"fixture-thread\"}}}\n", request.ID)
		} else if request.Method == "turn/start" {
			fmt.Printf("{\"id\":%s,\"result\":{}}\n", request.ID)
			message, _ := json.Marshal(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "fixture-thread", "item": map[string]any{"type": "agentMessage", "id": "fixture-message", "text": string(payload)}}})
			fmt.Println(string(message))
			fmt.Println(`{"method":"turn/completed","params":{"threadId":"fixture-thread","turn":{"id":"fixture-turn","status":"completed"}}}`)
		}
	}
}

func TestCredentialExclusiveProductionAdapters(test *testing.T) {
	if _, err := os.Stat("/usr/bin/bwrap"); os.IsNotExist(err) {
		test.Skip("credential-exclusive integration NOT-RUN: system bubblewrap is unavailable")
	}
	inheritedGateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		test.Error("ordinary fixture used an inherited provider gateway")
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer inheritedGateway.Close()
	test.Setenv("ANTHROPIC_BASE_URL", inheritedGateway.URL)
	test.Setenv("OPENAI_BASE_URL", inheritedGateway.URL)
	test.Setenv("OWNED_UNLIMITED", "owned-unlimited")
	test.Setenv("OPENAI_API_KEY", "owned-unlimited")
	test.Setenv("AWS_SECRET_ACCESS_KEY", "owned-unlimited")
	for _, provider := range []string{"claude", "codex"} {
		test.Run(provider, func(test *testing.T) {
			root := test.TempDir()
			outside := filepath.Join(root, "unlimited")
			if err := os.WriteFile(outside, []byte("owned-unlimited"), 0600); err != nil {
				test.Fatal(err)
			}
			executable := filepath.Join(root, "credential-fixture-"+provider)
			current, _ := os.Executable()
			binary, err := os.ReadFile(current)
			if err != nil {
				test.Fatal(err)
			}
			if err := os.WriteFile(executable, binary, 0700); err != nil {
				test.Fatal(err)
			}
			unmanaged := filepath.Join(root, "unmanaged")
			if err := os.Mkdir(unmanaged, 0700); err != nil {
				test.Fatal(err)
			}
			peerFile, err := os.Open(outside)
			if err != nil {
				test.Fatal(err)
			}
			defer peerFile.Close()
			peer := exec.Command(current, "__owned_credential_peer")
			peer.Env = []string{"OWNED_PEER_SECRET=owned-unlimited"}
			peer.ExtraFiles = []*os.File{peerFile}
			peerInput, err := peer.StdinPipe()
			if err != nil {
				test.Fatal(err)
			}
			if err := peer.Start(); err != nil {
				test.Fatal(err)
			}
			defer func() {
				_ = peerInput.Close()
				if err := peer.Wait(); err != nil {
					test.Error(err)
				}
			}()
			hostService := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(writer, "owned-unlimited") }))
			defer hostService.Close()
			socketDirectory, err := os.MkdirTemp("/tmp", "credential-peer-")
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() { _ = os.Remove(socketDirectory) })
			peerSocket := filepath.Join(socketDirectory, "peer.sock")
			abstractSocket := "@" + filepath.Base(socketDirectory)
			for _, socket := range []string{peerSocket, abstractSocket} {
				listener, err := net.Listen("unix", socket)
				if err != nil {
					test.Fatal(err)
				}
				server := &http.Server{Handler: hostService.Config.Handler}
				serveDone := make(chan struct{})
				go func() { defer close(serveDone); _ = server.Serve(listener) }()
				test.Cleanup(func() { _ = server.Close(); <-serveDone })
			}
			probe := credentialProbe{Outside: outside, OtherTask: filepath.Join(unmanaged, "auth.json"), PeerEnvironment: fmt.Sprintf("/proc/%d/environ", peer.Process.Pid), PeerDescriptor: fmt.Sprintf("/proc/%d/fd/3", peer.Process.Pid), HostURL: hostService.URL, PeerSocket: peerSocket, AbstractSocket: abstractSocket}
			if err := os.WriteFile(probe.OtherTask, []byte("owned-unlimited"), 0600); err != nil {
				test.Fatal(err)
			}
			writeCredentialProbe(test, unmanaged, probe)
			ordinary := observeCredentialExecution(test, provider, Config{ExecutablePath: executable, Env: map[string]string{"HOME": unmanaged}}, ExecOptions{Cwd: unmanaged})
			if !ordinary.Readable["direct"] || !ordinary.Readable["symlink"] || !ordinary.Readable["peer-env"] || !ordinary.Readable["peer-fd"] || !ordinary.Inherited || !ordinary.Helper || !ordinary.HostNetwork || !ordinary.UnixPeer || !ordinary.AbstractPeer {
				test.Fatalf("negative control did not expose owned fallback: %+v", ordinary)
			}
			if os.Getenv("MULTICA_FIXTURE_ASSERT_ORDINARY_ISOLATED") == "1" {
				test.Fatalf("INTENDED NEGATIVE-CONTROL FAILURE: ordinary native launch reads owned unlimited sentinel: %+v", ordinary)
			}
			test.Log("NEGATIVE CONTROL: ordinary production adapter reads owned unlimited sentinel and inherited credential")
			binding := credentialexec.Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"}
			spec := credentialexec.Spec{Root: filepath.Join(root, "private"), Binding: binding, Provider: provider, Executable: executable, HelperExecutable: current}
			spec.Inputs = map[string][]byte{"multica-input/prompt.md": []byte("owned authenticated prompt"), "multica-input/skills/owned/SKILL.md": []byte("owned authenticated skill")}
			boundary, err := credentialexec.Prepare(context.Background(), spec)
			if err != nil {
				test.Fatal(err)
			}
			defer boundary.Close()
			otherSpec := spec
			otherSpec.Binding.TaskID = "00000000-0000-4000-8000-000000000004"
			otherBoundary, err := credentialexec.Prepare(context.Background(), otherSpec)
			if err != nil {
				test.Fatal(err)
			}
			defer otherBoundary.Close()
			probe.OtherTask = filepath.Join(otherBoundary.Home(), "auth.json")
			if err := os.WriteFile(probe.OtherTask, []byte("owned-unlimited"), 0600); err != nil {
				test.Fatal(err)
			}
			writeCredentialProbe(test, boundary.WorkDir(), probe)
			gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("X-Multica-Task-Id") != binding.TaskID || request.Header.Get("X-Api-Key") != "owned-scoped" && request.Header.Get("Authorization") != "Bearer owned-scoped" {
					test.Error("gateway did not receive authoritative task-bound credentials")
					writer.WriteHeader(403)
					return
				}
				_, _ = io.WriteString(writer, "owned-scoped")
			}))
			defer gateway.Close()
			if err := boundary.BindGateway(context.Background(), credentialexec.GatewayCredential{Binding: binding, BaseURL: gateway.URL, Key: "owned-scoped"}); err != nil {
				test.Fatal(err)
			}
			config := Config{ExecutablePath: executable, TaskID: binding.TaskID, BuiltinRuntime: true, RequireCredentialIsolation: true, CredentialBoundary: boundary, Env: map[string]string{"OPENAI_API_KEY": "owned-unlimited", "HOME": unmanaged, "AWS_SECRET_ACCESS_KEY": "owned-unlimited"}}
			for attempt := 1; attempt <= 2; attempt++ {
				options := ExecOptions{Cwd: boundary.WorkDir()}
				if attempt == 2 {
					options.ResumeSessionID = "fixture-thread"
				}
				isolated := observeCredentialExecution(test, provider, config, options)
				for route, readable := range isolated.Readable {
					if readable {
						test.Errorf("isolated %s reads %s", provider, route)
					}
				}
				if isolated.Inherited || isolated.Helper || isolated.DescriptorLeak || isolated.HostNetwork || isolated.UnixPeer || isolated.AbstractPeer || isolated.ParentLeak || !isolated.Scoped || !isolated.Gateway || !isolated.AdminDenied || !isolated.NativeControl || !isolated.StagedInputs || isolated.Count != attempt {
					test.Fatalf("isolated control: %+v, attempt %d", isolated, attempt)
				}
			}
			for _, options := range []ExecOptions{{Cwd: boundary.WorkDir(), CustomArgs: []string{"--profile", "fallback"}}, {Cwd: boundary.WorkDir(), ExtraArgs: []string{"--config", "auth=fallback"}}, {Cwd: boundary.WorkDir(), McpConfig: json.RawMessage(`{}`)}, {Cwd: unmanaged}} {
				backend, err := New(provider, config)
				if err != nil {
					test.Fatal(err)
				}
				if _, err := backend.Execute(context.Background(), "must not launch", options); err == nil {
					test.Error("override route was not refused")
				}
			}
			command := exec.Command(executable)
			command.Dir = boundary.WorkDir()
			command.ExtraFiles = []*os.File{peerFile}
			if _, err := boundary.Wrap(command); err == nil {
				test.Error("inherited descriptor accepted")
			}
			if err := boundary.BindGateway(context.Background(), credentialexec.GatewayCredential{Binding: binding, BaseURL: gateway.URL, Key: "different"}); err == nil {
				test.Error("same-task credential rebound")
			}
			test.Log("ISOLATED: direct/symlink/cross-task/env denied; scoped gateway and same-task state pass")
		})
	}
}

func writeCredentialProbe(test *testing.T, workdir string, probe credentialProbe) {
	test.Helper()
	data, _ := json.Marshal(probe)
	if err := os.WriteFile(filepath.Join(workdir, "probe.json"), data, 0600); err != nil {
		test.Fatal(err)
	}
	if err := os.Symlink(probe.Outside, filepath.Join(workdir, "escape")); err != nil {
		test.Fatal(err)
	}
}

func observeCredentialExecution(test *testing.T, provider string, config Config, options ExecOptions) credentialObservation {
	test.Helper()
	backend, err := New(provider, config)
	if err != nil {
		test.Fatal(err)
	}
	options.Timeout = 10 * time.Second
	options.HandshakeTimeout = 4 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "owned fixture", options)
	if err != nil {
		test.Fatal(err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	result := <-session.Result
	if result.Status != "completed" {
		test.Fatalf("adapter failed: %+v", result)
	}
	var observation credentialObservation
	if err := json.Unmarshal([]byte(result.Output), &observation); err != nil {
		test.Fatalf("observation %q: %v", result.Output, err)
	}
	return observation
}

func TestCredentialExclusiveAbsentBoundaryRefusesBeforeNativeLaunch(test *testing.T) {
	for _, provider := range []string{"claude", "codex", "hermes"} {
		if _, err := New(provider, Config{ExecutablePath: "/owned/not-launched", RequireCredentialIsolation: true, BuiltinRuntime: true}); err == nil {
			test.Errorf("%s accepted missing boundary", provider)
		}
	}
}

func TestCredentialFixtureDescriptorProbeDoesNotBlock(test *testing.T) {
	executable := filepath.Join(test.TempDir(), "credential-fixture-descriptor")
	current, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	binary, err := os.ReadFile(current)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(executable, binary, 0700); err != nil {
		test.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		test.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "__owned_descriptor_helper")
	command.Env = []string{"GORACE=atexit_sleep_ms=0"}
	command.ExtraFiles = []*os.File{reader}
	if output, err := command.CombinedOutput(); err != nil || len(output) != 0 {
		test.Fatalf("descriptor probe blocked or read a nonregular descriptor: %q, %v", output, err)
	}
}
