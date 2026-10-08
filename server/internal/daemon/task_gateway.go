package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/taskgateway"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func (client *Client) PrepareTaskGateway(ctx context.Context, task Task, spec credentialexec.Spec) (*credentialexec.Boundary, error) {
	boundary, _, err := client.prepareTaskGateway(ctx, task, spec)
	return boundary, err
}

func (client *Client) prepareTaskGateway(ctx context.Context, task Task, spec credentialexec.Spec) (*credentialexec.Boundary, Task, error) {
	if client == nil || !task.RequireCredentialIsolation || task.CredentialExecutionBinding == nil || *task.CredentialExecutionBinding != spec.Binding || spec.Binding.Validate() != nil || task.ID != spec.Binding.TaskID || task.WorkspaceID != spec.Binding.WorkspaceID || !strings.HasPrefix(task.TaskGatewayDaemonToken, "mdt_") || !strings.HasPrefix(task.AuthToken, "mat_") || strings.ContainsAny(task.TaskGatewayDaemonToken+task.AuthToken, "\r\n\x00") {
		return nil, Task{}, taskgateway.ErrUnavailable
	}
	runtimeID, err := uuid.Parse(task.RuntimeID)
	if err != nil || runtimeID == uuid.Nil || runtimeID.String() != task.RuntimeID {
		return nil, Task{}, taskgateway.ErrUnavailable
	}
	if _, err := time.Parse(time.RFC3339Nano, task.DispatchedAt); err != nil {
		return nil, Task{}, taskgateway.ErrUnavailable
	}
	if len(spec.Inputs) != 0 {
		return nil, Task{}, taskgateway.ErrUnavailable
	}
	endpoint, err := url.Parse(client.baseURL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Opaque != "" || endpoint.Path != "" && endpoint.Path != "/" {
		return nil, Task{}, taskgateway.ErrUnavailable
	}
	address := net.ParseIP(endpoint.Hostname())
	if endpoint.Scheme != "https" && (endpoint.Scheme != "http" || address == nil || !address.IsLoopback()) {
		return nil, Task{}, taskgateway.ErrUnavailable
	}
	task, err = client.resolveTaskGatewaySkills(ctx, task, spec.Provider)
	if err != nil {
		return nil, Task{}, taskgateway.ErrUnavailable
	}
	spec.Inputs, err = taskGatewayInputs(task, spec.Provider)
	if err != nil {
		return nil, Task{}, taskgateway.ErrUnavailable
	}
	boundary, err := taskgateway.PrepareHandoff(ctx, spec, func(ctx context.Context) (taskgateway.Grant, error) {
		body, err := json.Marshal(struct {
			TaskToken    string `json:"task_token"`
			DispatchedAt string `json:"dispatched_at"`
		}{task.AuthToken, task.DispatchedAt})
		if err != nil {
			return taskgateway.Grant{}, taskgateway.ErrUnavailable
		}
		contents, err := client.taskGatewayRequest(ctx, task, "gateway-grant", body, taskgateway.HandoffLimit)
		if err != nil {
			return taskgateway.Grant{}, taskgateway.ErrUnavailable
		}
		return taskgateway.DecodeHandoff(contents, spec.Binding)
	})
	return boundary, task, err
}

func (client *Client) taskGatewayRequest(ctx context.Context, task Task, route string, body []byte, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(client.baseURL, "/")+"/api/daemon/runtimes/"+task.RuntimeID+"/tasks/"+task.ID+"/"+route, bytes.NewReader(body))
	if err != nil {
		return nil, taskgateway.ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+task.TaskGatewayDaemonToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Client-Capabilities", taskgateway.Capability)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Timeout: 25 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, taskgateway.ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, taskgateway.ErrUnavailable
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(contents)) > limit {
		return nil, taskgateway.ErrUnavailable
	}
	return contents, nil
}
