package handler

import (
	"context"
	"log/slog"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (handler *Handler) enqueueChatCompactionAfterCompletion(ctx context.Context, task db.AgentTaskQueue) {
	if _, err := handler.TaskService.MaybeEnqueueChatCompaction(ctx, task); err != nil {
		slog.Warn("chat auto compaction admission failed", "task_id", uuidToString(task.ID), "error", err)
	}
}
