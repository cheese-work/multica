// Event types handled by specific handlers in use-realtime-sync -- the generic
// refresh skips them. Every value must be a Go Event* const
// (server/internal/service/web_client_lists_contract_test.go).
export const specificEvents = new Set([
  "workspace:updated",
  "issue:updated", "issue:created", "issue:deleted", "issue_attachments:changed", "issue_labels:changed", "issue_unread:changed", "issue_metadata:changed", "issue_properties:changed", "property:created", "property:updated", "inbox:new",
  "comment:created", "comment:updated", "comment:deleted",
  "comment:resolved", "comment:unresolved", "decision_answer:updated",
  "activity:created",
  "reaction:added", "reaction:removed",
  "issue_reaction:added", "issue_reaction:removed",
  "subscriber:added", "subscriber:removed",
  "daemon:heartbeat",
  // Chat events are handled explicitly below; do not double-invalidate.
  "chat:message", "chat:done", "chat:quick_actions", "chat:cancel_finalized", "chat:session_read", "chat:supplement_updated",
  "chat:session_created", "chat:session_deleted", "chat:session_updated",
  // task:message stays out of the prefix path because it fires per
  // streamed message during a long run — invalidating the snapshot on
  // every message would flood the network. Specific chat handlers below
  // still receive it via ws.on() (a separate subscription channel).
  "task:message",
  // task:completed / task:failed deliberately NOT here. They go through
  // both the task-prefix invalidate (refreshes the agent-task-snapshot
  // cache) AND the chat-specific ws.on() handlers below. The two
  // channels are independent — onAny dispatch and ws.on are separate
  // subscriptions.
]);
