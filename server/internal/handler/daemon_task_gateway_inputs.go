package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/taskgateway"
)

func (handler *Handler) DeliverTaskGatewayInputs(writer http.ResponseWriter, request *http.Request) {
	handler.deliverTaskGatewayClaim(writer, request, func(ctx context.Context, claim taskGatewayClaim) ([]byte, error) {
		for _, contents := range [][]byte{claim.agent.CustomArgs, claim.agent.CustomEnv, claim.agent.McpConfig, claim.agent.RuntimeConfig, claim.agent.DisabledRuntimeSkills} {
			if len(bytes.TrimSpace(contents)) == 0 {
				continue
			}
			var configuration any
			if json.Unmarshal(contents, &configuration) != nil {
				return nil, taskgateway.ErrUnavailable
			}
			switch configuration := configuration.(type) {
			case nil:
			case []any:
				if len(configuration) != 0 {
					return nil, taskgateway.ErrUnavailable
				}
			case map[string]any:
				if len(configuration) != 0 {
					return nil, taskgateway.ErrUnavailable
				}
			default:
				return nil, taskgateway.ErrUnavailable
			}
		}
		workspace, err := claim.queries.GetWorkspace(ctx, claim.runtime.WorkspaceID)
		if err != nil || workspace.ID != claim.runtime.WorkspaceID {
			return nil, taskgateway.ErrUnavailable
		}
		return taskgateway.EncodeInputSnapshot(taskgateway.InputSnapshot{
			Binding: claim.binding, RuntimeID: uuidToString(claim.runtime.ID), AgentID: uuidToString(claim.agent.ID),
			DispatchedAt: claim.task.DispatchedAt.Time.UTC().Format(time.RFC3339Nano),
			Instructions: claim.agent.Instructions, WorkspaceContext: workspace.Context.String,
		})
	})
}
