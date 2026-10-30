package main

import (
	"context"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// registerMachinePauseListeners keeps the Machine card and banner honest in
// every workspace a paused machine serves (CHE-2430). A task starting or ending
// changes the drain count, but its event reaches only the task's own workspace;
// the machine's other workspaces get a runtime refresh. Nothing is sent for a
// machine that is not paused.
func registerMachinePauseListeners(bus *events.Bus, queries *db.Queries) {
	for _, eventType := range []string{
		protocol.EventTaskDispatch, protocol.EventTaskCompleted,
		protocol.EventTaskFailed, protocol.EventTaskCancelled,
	} {
		bus.Subscribe(eventType, func(e events.Event) {
			// The dispatch event carries the id in its payload only.
			taskID := e.TaskID
			if payload, ok := e.Payload.(map[string]any); ok && taskID == "" {
				taskID, _ = payload["task_id"].(string)
			}
			if taskID == "" {
				return
			}
			handler.RefreshMachineOfTask(context.Background(), queries, bus, taskID, e.WorkspaceID)
		})
	}
}
