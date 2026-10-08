package credentialexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const HelperArg = "__multica_credential_runner"
const NativePlaceholder = "multica-task-gateway-only"

var ErrUnavailable = errors.New("credential execution boundary unavailable")
var ErrQuotaExhausted = fmt.Errorf("%w: task quota exhausted", ErrUnavailable)
var ErrOutcomeUnknown = fmt.Errorf("%w: task gateway outcome unknown", ErrUnavailable)

type Binding struct {
	TaskID      string `json:"task_id"`
	OwnerID     string `json:"owner_id"`
	WorkspaceID string `json:"workspace_id"`
}

type Spec struct {
	Root             string
	Binding          Binding
	Provider         string
	Executable       string
	HelperExecutable string
	Inputs           map[string][]byte
}

type GatewayCredential struct {
	Binding Binding
	BaseURL string
	Key     string
}

type Boundary struct {
	spec         Spec
	state        string
	runtimeRoot  string
	bwrap        string
	mutex        sync.Mutex
	server       *http.Server
	listener     net.Listener
	socket       string
	credential   GatewayCredential
	closed       bool
	serveDone    chan struct{}
	transport    *http.Transport
	requestMutex sync.Mutex
	stopErr      error
	stopped      chan struct{}
}

func (binding Binding) Validate() error {
	for _, value := range []string{binding.TaskID, binding.OwnerID, binding.WorkspaceID} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed == uuid.Nil || parsed.String() != value {
			return fmt.Errorf("%w: noncanonical task/owner/workspace identity", ErrUnavailable)
		}
	}
	return nil
}

func (boundary *Boundary) WorkDir() string { return filepath.Join(boundary.state, "workdir") }
func (boundary *Boundary) Home() string    { return filepath.Join(boundary.state, "home") }

func (boundary *Boundary) Validate(taskID, provider, executable string) error {
	if boundary == nil {
		return fmt.Errorf("%w: opted-in launch has no prepared boundary", ErrUnavailable)
	}
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	if err := boundary.stopErrorLocked(); err != nil {
		return err
	}
	if boundary.closed || boundary.spec.Binding.TaskID != taskID || boundary.spec.Provider != provider || boundary.spec.Executable != executable {
		return fmt.Errorf("%w: launch binding mismatch or closed boundary", ErrUnavailable)
	}
	return nil
}

func (boundary *Boundary) Environment() map[string]string {
	return map[string]string{
		"HOME": boundary.Home(), "PATH": "/bin", "LANG": "C.UTF-8",
		"XDG_CONFIG_HOME":   filepath.Join(boundary.Home(), ".config"),
		"XDG_CACHE_HOME":    filepath.Join(boundary.Home(), ".cache"),
		"XDG_DATA_HOME":     filepath.Join(boundary.Home(), ".data"),
		"CODEX_HOME":        filepath.Join(boundary.Home(), ".codex"),
		"CLAUDE_CONFIG_DIR": filepath.Join(boundary.Home(), ".claude"),
		"OPENAI_API_KEY":    NativePlaceholder, "ANTHROPIC_API_KEY": NativePlaceholder,
		"MULTICA_CREDENTIAL_PROVIDER": boundary.spec.Provider,
		"MULTICA_TASK_ID":             boundary.spec.Binding.TaskID,
		"AWS_EC2_METADATA_DISABLED":   "true",
	}
}

func (boundary *Boundary) BindGateway(ctx context.Context, credential GatewayCredential) error {
	if err := boundary.Probe(ctx); err != nil {
		return err
	}
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if boundary.closed || credential.Binding != boundary.spec.Binding || credential.Key == "" || strings.ContainsAny(credential.Key, "\r\n\x00") {
		return fmt.Errorf("%w: gateway credential binding rejected", ErrUnavailable)
	}
	upstream, err := url.Parse(credential.BaseURL)
	if err != nil || upstream.Host == "" || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" || upstream.Path != "" && upstream.Path != "/" || upstream.Scheme != "http" && upstream.Scheme != "https" {
		return fmt.Errorf("%w: unsupported gateway endpoint", ErrUnavailable)
	}
	if boundary.server != nil {
		if credential != boundary.credential {
			return fmt.Errorf("%w: same-task gateway cannot be rebound", ErrUnavailable)
		}
		return nil
	}
	digest := sha256.Sum256([]byte(credential.Key))
	identity, err := json.Marshal(struct {
		Binding Binding
		BaseURL string
		Digest  string
	}{credential.Binding, credential.BaseURL, hex.EncodeToString(digest[:])})
	if err != nil {
		return err
	}
	identityPath := filepath.Join(boundary.state, "gateway.json")
	identityFile, err := os.OpenFile(identityPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		existing, readErr := os.ReadFile(identityPath)
		if readErr != nil || string(existing) != string(identity) {
			return fmt.Errorf("%w: persisted gateway binding cannot change", ErrUnavailable)
		}
	} else if err != nil {
		return fmt.Errorf("%w: record gateway binding", ErrUnavailable)
	} else {
		_, writeErr := identityFile.Write(identity)
		closeErr := identityFile.Close()
		if writeErr != nil || closeErr != nil {
			return fmt.Errorf("%w: persist gateway binding", ErrUnavailable)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	brokerDir, err := os.MkdirTemp("/tmp", "multica-gateway-")
	if err != nil {
		return fmt.Errorf("%w: create gateway broker", ErrUnavailable)
	}
	socket := filepath.Join(brokerDir, "gateway.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.Remove(brokerDir)
		return fmt.Errorf("%w: create gateway socket", ErrUnavailable)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(brokerDir)
		return fmt.Errorf("%w: secure gateway socket", ErrUnavailable)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			request.Out.Header.Del("Authorization")
			request.Out.Header.Del("X-Api-Key")
			request.Out.Header.Del("Cookie")
			request.Out.Header.Del("Proxy-Authorization")
			request.Out.Header.Set("X-Multica-Task-Id", boundary.spec.Binding.TaskID)
			if boundary.spec.Provider == "claude" {
				request.Out.Header.Set("X-Api-Key", credential.Key)
			} else {
				request.Out.Header.Set("Authorization", "Bearer "+credential.Key)
			}
		},
		Transport: &gatewayTransport{boundary: boundary, transport: transport},
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(writer, "task gateway unavailable", http.StatusBadGateway)
		},
		FlushInterval: -1,
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 * 1024, Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !allowedGatewayRequest(boundary.spec.Provider, request) {
			http.Error(writer, "unsupported task gateway route", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(writer, request)
	})}
	boundary.socket, boundary.listener, boundary.server, boundary.credential = socket, listener, server, credential
	boundary.transport = transport
	boundary.serveDone = make(chan struct{})
	go func() { defer close(boundary.serveDone); _ = server.Serve(listener) }()
	return nil
}

func allowedGatewayRequest(provider string, request *http.Request) bool {
	if request.URL.IsAbs() || request.URL.RawPath != "" {
		return false
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v1/models" {
		return true
	}
	if request.Method != http.MethodPost {
		return false
	}
	if provider == "claude" {
		return request.URL.Path == "/v1/messages" || request.URL.Path == "/v1/messages/count_tokens"
	}
	return request.URL.Path == "/v1/responses"
}

func (boundary *Boundary) Close() error {
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	boundary.closed = true
	if boundary.server == nil {
		return nil
	}
	err := boundary.server.Close()
	<-boundary.serveDone
	boundary.transport.CloseIdleConnections()
	_ = os.Remove(filepath.Dir(boundary.socket))
	return err
}

func RunHelper(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("credential runner: missing native executable")
	}
	socket := os.Getenv("MULTICA_CREDENTIAL_GATEWAY_SOCKET")
	if socket == "" {
		return errors.New("credential runner: missing scoped gateway")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errors.New("credential runner: loopback listener unavailable")
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	proxy := &httputil.ReverseProxy{Rewrite: func(request *httputil.ProxyRequest) {
		request.Out.URL.Scheme = "http"
		request.Out.URL.Host = "task-gateway"
	}, Transport: transport, FlushInterval: -1,
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(writer, "task gateway unavailable", http.StatusBadGateway)
		}}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 * 1024}
	serveDone := make(chan struct{})
	defer func() { _ = server.Close(); <-serveDone; transport.CloseIdleConnections() }()
	go func() { defer close(serveDone); _ = server.Serve(listener) }()
	environment := os.Environ()
	baseURL := "http://" + listener.Addr().String()
	environment = append(environment, "ANTHROPIC_BASE_URL="+baseURL, "OPENAI_BASE_URL="+baseURL+"/v1")
	command := exec.Command(arguments[0], arguments[1:]...)
	command.Env = environment
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return errors.New("credential runner: native process failed")
	}
	return nil
}
