package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/multica-ai/multica/server/internal/taskgateway"
)

func (client *Client) refreshTaskGatewayInputs(ctx context.Context, task Task, provider string) (Task, error) {
	if validateTaskIdentity(task) != nil || task.CredentialExecutionBinding == nil || client.validateTaskGatewayClaim(task, *task.CredentialExecutionBinding) != nil {
		return Task{}, taskgateway.ErrUnavailable
	}
	body, err := json.Marshal(struct {
		TaskToken    string `json:"task_token"`
		DispatchedAt string `json:"dispatched_at"`
	}{task.AuthToken, task.DispatchedAt})
	if err != nil {
		return Task{}, taskgateway.ErrUnavailable
	}
	contents, err := client.taskGatewayRequest(ctx, task, "gateway-inputs", body, taskgateway.InputSnapshotLimit)
	if err != nil {
		return Task{}, taskgateway.ErrUnavailable
	}
	snapshot, err := taskgateway.DecodeInputSnapshot(contents)
	if err != nil || snapshot.Binding != *task.CredentialExecutionBinding || snapshot.RuntimeID != task.RuntimeID || snapshot.AgentID != task.AgentID {
		return Task{}, taskgateway.ErrUnavailable
	}
	expected, err := time.Parse(time.RFC3339Nano, task.DispatchedAt)
	actual, actualErr := time.Parse(time.RFC3339Nano, snapshot.DispatchedAt)
	if err != nil || actualErr != nil || !expected.Equal(actual) {
		return Task{}, taskgateway.ErrUnavailable
	}
	refreshed := task
	refreshedAgent := *task.Agent
	refreshed.Agent = &refreshedAgent
	refreshed.Agent.Instructions, refreshed.WorkspaceContext = snapshot.Instructions, snapshot.WorkspaceContext
	return client.resolveTaskGatewaySkills(ctx, refreshed, provider)
}

func (execution *taskGatewayExecution) Refresh(ctx context.Context) error {
	runCtx, finish, err := execution.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if execution.boundary.VerifyInputs(runCtx) != nil {
		return taskgateway.ErrUnavailable
	}
	refreshed, err := execution.client.refreshTaskGatewayInputs(runCtx, execution.task, execution.provider)
	if err != nil {
		return taskgateway.ErrUnavailable
	}
	inputs, err := taskGatewayInputs(refreshed, execution.provider)
	if err != nil || execution.boundary.RefreshInputs(runCtx, *execution.task.CredentialExecutionBinding, inputs) != nil {
		return taskgateway.ErrUnavailable
	}
	execution.prompt = BuildPrompt(refreshed, execution.provider)
	return nil
}
