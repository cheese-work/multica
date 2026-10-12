package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func ValidateAutoCompactConfig(value any) error {
	settings, ok := value.(map[string]any)
	if !ok || settings["auto_compact"] == nil {
		return nil
	}
	setting, ok := settings["auto_compact"].(string)
	if !ok {
		return fmt.Errorf("auto_compact must be off or threshold:<percent>")
	}
	_, err := chatCompactionThreshold(setting)
	return err
}

func chatCompactionThreshold(setting string) (int64, error) {
	if setting == "" || setting == "off" {
		return 0, nil
	}
	value, found := strings.CutPrefix(setting, "threshold:")
	percent, err := strconv.ParseInt(value, 10, 64)
	if !found || err != nil || percent < 1 || percent > 89 {
		return 0, fmt.Errorf("auto_compact must be off or threshold:1 through threshold:89")
	}
	return percent, nil
}

func ShouldAutoCompact(config []byte, request *agent.ModelRequestUsage) bool {
	var settings struct {
		AutoCompact string `json:"auto_compact"`
	}
	if json.Unmarshal(config, &settings) != nil || request == nil || request.Validate() != nil {
		return false
	}
	percent, err := chatCompactionThreshold(settings.AutoCompact)
	if err != nil || percent == 0 || request.ContextWindow <= 0 || request.CLICompactionTokens <= 0 {
		return false
	}
	threshold := (request.ContextWindow*percent + 99) / 100
	return threshold < request.CLICompactionTokens && request.InputSize() >= threshold
}

func (service *TaskService) MaybeEnqueueChatCompaction(ctx context.Context, completed db.AgentTaskQueue) (*db.AgentTaskQueue, error) {
	if completed.Status != "completed" || !completed.ChatSessionID.Valid || completed.ChatCompaction || len(completed.LastModelRequest) == 0 {
		return nil, nil
	}
	var request agent.ModelRequestUsage
	if json.Unmarshal(completed.LastModelRequest, &request) != nil {
		return nil, nil
	}
	session, err := service.Queries.GetChatSession(ctx, completed.ChatSessionID)
	if err != nil {
		return nil, err
	}
	carrier, err := service.Queries.GetAgent(ctx, session.AgentID)
	if err != nil {
		return nil, err
	}
	if !ShouldAutoCompact(carrier.RuntimeConfig, &request) {
		return nil, nil
	}
	prepared, err := service.PrepareChatTaskEnqueue(ctx, session.AgentID, session.CreatorID)
	if err != nil {
		return nil, err
	}
	var compactTask *db.AgentTaskQueue
	err = service.runInTx(ctx, func(queries *db.Queries) error {
		current, err := queries.LockChatSessionForEnqueue(ctx, session.ID)
		if err != nil {
			return err
		}
		if current.Status != "active" || !current.SessionID.Valid {
			return nil
		}
		carrier, err := queries.GetAgentForClaimUpdate(ctx, current.AgentID)
		if err != nil {
			return err
		}
		if !ShouldAutoCompact(carrier.RuntimeConfig, &request) {
			return nil
		}
		if _, err := queries.GetChannelChatSessionBindingBySessionAny(ctx, current.ID); !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		pending, err := queries.HasPendingChatTurnForSession(ctx, current.ID)
		if err != nil || pending {
			return err
		}
		latest, err := queries.IsLatestChatTask(ctx, completed.ID)
		if err != nil || !latest {
			return err
		}
		allowed, err := memberMayInvokeAgent(ctx, queries, carrier, current.CreatorID, current.WorkspaceID)
		if err != nil || !allowed {
			return err
		}
		task, err := service.enqueueChatTaskTx(ctx, queries, current, current.CreatorID, false, 0, false, pgtype.UUID{}, 0, prepared)
		if err != nil {
			return err
		}
		task, err = queries.SetChatTaskCompaction(ctx, task.ID)
		if err != nil {
			return err
		}
		compactTask = &task
		return nil
	})
	if err == nil && compactTask != nil {
		service.FinalizeChatTaskEnqueue(ctx, *compactTask)
	}
	return compactTask, err
}
