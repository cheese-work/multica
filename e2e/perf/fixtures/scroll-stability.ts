/**
 * Fixture for the scroll-stability scenarios (CHE-3231, A0).
 *
 * Synthetic and deterministic: a DM with a long history served by a real
 * cursor-paginated endpoint, and an issue with a long comment thread. No
 * account, workspace data or captured transcript.
 */
export const WORKSPACE_ID = "11111111-1111-4111-8111-111111111111";
export const WORKSPACE_SLUG = "perf";
export const USER_ID = "22222222-2222-4222-8222-222222222222";
export const AGENT_ID = "33333333-3333-4333-8333-333333333333";
export const SESSION_ID = "44444444-4444-4444-8444-444444444444";
export const ISSUE_ID = "55555555-5555-4555-8555-555555555555";
export const ISSUE_IDENTIFIER = "PERF-1";
/** The issue the DM's decision card is about. */
export const CARD_ISSUE_ID = "66666666-6666-4666-8666-666666666666";

export const CHAT_MESSAGE_COUNT = 150;
export const PAGE_SIZE = 50;
/** Index (oldest = 0) of the message that holds the decision card. */
export const CARD_MESSAGE_INDEX = 100;
export const COMMENT_COUNT = 45;

const BASE_MS = Date.UTC(2026, 9, 1, 0, 0, 0);
export const at = (minutes: number) => new Date(BASE_MS + minutes * 60_000).toISOString();

const timestamp = at(0);

export const workspace = {
  id: WORKSPACE_ID, name: "Perf", slug: WORKSPACE_SLUG, description: null, context: null,
  settings: { github_enabled: false }, repos: [], issue_prefix: "PERF", avatar_url: null,
  created_at: timestamp, updated_at: timestamp,
};

export const me = {
  id: USER_ID, name: "Perf User", email: "perf@example.test", avatar_url: null,
  language: "en", onboarded_at: timestamp, onboarding_questionnaire: { source: "other" },
  created_at: timestamp, updated_at: timestamp,
};

export const agent = {
  id: AGENT_ID, workspace_id: WORKSPACE_ID, runtime_id: "77777777-7777-4777-8777-777777777777",
  name: "Fixture Agent", description: "", instructions: "", avatar_url: null,
  runtime_mode: "local", runtime_config: {}, custom_args: [], visibility: "workspace",
  permission_mode: "public_to", invocation_targets: [{ target_type: "workspace", target_id: null }],
  status: "idle", max_concurrent_tasks: 1, model: "", owner_id: USER_ID, skills: [],
  runtime_availability: "online", created_at: timestamp, updated_at: timestamp,
  archived_at: null, archived_by: null,
};

export const session = {
  id: SESSION_ID, workspace_id: WORKSPACE_ID, agent_id: AGENT_ID, creator_id: USER_ID,
  title: "Scroll stability", status: "active", has_unread: false, unread_count: 0,
  last_message: null, pinned: false, created_at: timestamp, updated_at: timestamp,
};

export const issue = {
  id: ISSUE_ID, workspace_id: WORKSPACE_ID, number: 1, identifier: ISSUE_IDENTIFIER,
  title: "Scroll stability", description: "Performance scenario fixture.",
  status: "in_progress", status_category: "in_progress", status_name: "In Progress",
  priority: "none", assignee_type: null, assignee_id: null, creator_type: "user",
  creator_id: USER_ID, parent_issue_id: null, project_id: null, position: 0,
  start_date: null, due_date: null, stage: null, metadata: {}, properties: {},
  labels: [], reactions: [], created_at: timestamp, updated_at: timestamp,
};

export interface FixtureMessage {
  id: string;
  chat_session_id: string;
  role: "user" | "assistant";
  content: string;
  task_id: null;
  created_at: string;
  decision_request?: unknown;
}

export const messageId = (index: number) =>
  `aaaa${String(index).padStart(4, "0")}-4444-4444-8444-444444444444`;

/** Mixed heights: one to five sentences, so rows are not interchangeable. */
export function chatMessage(index: number): FixtureMessage {
  const role = index % 3 === 0 ? "user" : "assistant";
  const base: FixtureMessage = {
    id: messageId(index), chat_session_id: SESSION_ID, role, task_id: null,
    content: `Message ${index}. `.concat("The reviewer walks through the change. ".repeat(1 + (index % 5))),
    created_at: at(index),
  };
  if (index !== CARD_MESSAGE_INDEX) return base;
  return {
    ...base, role: "assistant", content: "",
    decision_request: {
      id: "bbbb0000-4444-4444-8444-444444444444", issue_id: CARD_ISSUE_ID,
      issue_identifier: "PERF-9", ask: "Which option should the run take?",
      options: [{ label: "Option A" }, { label: "Option B" }], state: "answered",
      created_at: base.created_at,
      answer: { state: "delivered", chosen_option_index: 0, option_label: "Option A", answered_at: base.created_at },
    },
  };
}

export const initialMessages = () =>
  Array.from({ length: CHAT_MESSAGE_COUNT }, (_, i) => chatMessage(i));

/** Same contract as GET /api/chat/sessions/:id/messages/page: newest page first, cursor walks back. */
export function messagesPage(all: FixtureMessage[], params: URLSearchParams) {
  const limit = Number(params.get("limit") ?? PAGE_SIZE);
  const beforeId = params.get("before_id");
  const end = beforeId ? all.findIndex((m) => m.id === beforeId) : all.length;
  const start = Math.max(0, end - limit);
  const messages = all.slice(start, end);
  const oldest = messages[0];
  return {
    messages, limit, has_more: start > 0,
    next_cursor: start > 0 && oldest ? { created_at: oldest.created_at, id: oldest.id } : null,
  };
}

/** Prose plus a highlighted code block per comment, as the long threads have. */
function commentBody(index: number): string {
  const paragraphs = Array.from({ length: 6 }, (_, p) =>
    `Paragraph ${p + 1} of comment ${index}. The reviewer walks through the change and what was measured.`);
  const code = [
    "```ts", `export function handler${index}(input: Record<string, unknown>) {`,
    "  const entries = Object.entries(input);",
    "  return entries.map(([key, value]) => ({ key, value }));", "}", "```",
  ].join("\n");
  return `${paragraphs.join("\n\n")}\n\n${code}`;
}

export const commentId = (index: number) =>
  `cccc${String(index).padStart(4, "0")}-4444-4444-8444-444444444444`;

/** Root comments only, so every comment is one row of the virtualized thread. */
export const timelineEntries = () =>
  Array.from({ length: COMMENT_COUNT }, (_, index) => ({
    type: "comment" as const, id: commentId(index), actor_type: "user" as const,
    actor_id: USER_ID, created_at: at(index), updated_at: at(index), issue_id: ISSUE_ID,
    parent_id: null, content: commentBody(index), resolved_at: null, reactions: [],
    attachments: [], source_task_id: null,
  }));

/** Requests both pages make on load, answered empty. */
export function commonResponse(pathname: string): unknown | undefined {
  switch (pathname) {
    case "/api/config": return { feature_flags: {}, allow_signup: true };
    case "/api/me": return me;
    case "/api/workspaces": return [workspace];
    case "/api/agents": return [agent];
    case `/api/workspaces/${WORKSPACE_ID}/members`: return [{
      id: "member-1", user_id: USER_ID, workspace_id: WORKSPACE_ID, role: "owner",
      name: "Perf User", email: "perf@example.test", avatar_url: null,
    }];
    case "/api/projects": return { projects: [], total: 0 };
    case "/api/issues": return { issues: [], total: 0 };
    case "/api/issue-statuses": return { statuses: [] };
    case "/api/properties": return { properties: [], total: 0 };
    case "/api/quick-actions": return { quick_actions: [] };
    case "/api/client-usage": return {};
    case "/api/issue-view-preferences": return {
      scope_type: "workspace", scope_id: null, ordered_view_ids: [], hidden_view_ids: [], default_view_id: null,
    };
    case "/api/issue-workflows": return { workflows: [] };
    case "/api/search-index/manifest": return {
      cursor: "0", issue_count: 0, comment_count: 0, project_count: 0, text_bytes: 0,
    };
    case "/api/search-index/snapshot": return {
      issues: [], comments: [], projects: [], next_after_number: 0, done: true,
    };
    case "/api/search-index/changes": return {
      issues: [], comments: [], projects: [], deleted: { issues: [], comments: [], projects: [] },
      cursor: "0", has_more: false,
    };
    case "/api/runtimes": case "/api/squads": case "/api/agent-task-snapshot":
    case "/api/invitations": case "/api/inbox": case "/api/inbox/unread-summary":
    case "/api/pins": case "/api/chat/pending-tasks": case "/api/assignee-frequency":
    case "/api/issue-views": case "/api/issue-wakeup-summaries":
    case "/api/agent-run-counts": case "/api/agent-activity-30d":
    case "/api/decisions/open":
      return [];
    case "/api/chat/pending-tasks/has-any": return { has_pending: false };
    default: return undefined;
  }
}

/** Requests the issue page makes. */
export function issueResponse(pathname: string): unknown | undefined {
  switch (pathname) {
    case `/api/issues/${ISSUE_IDENTIFIER}`:
    case `/api/issues/${ISSUE_ID}`: return issue;
    case `/api/issues/${ISSUE_ID}/timeline`: return timelineEntries();
    case `/api/issues/${ISSUE_ID}/comments/trigger-preview`: return { agents: [], blocked: [] };
    case "/api/issues/child-progress": return { progress: [] };
    case `/api/issues/${ISSUE_ID}/task-runs`:
    case `/api/issues/${ISSUE_ID}/subscribers`:
    case `/api/issues/${ISSUE_ID}/wakeups`:
    case `/api/issues/${ISSUE_ID}/attachments`: return [];
    case `/api/issues/${ISSUE_ID}/labels`: return { labels: [] };
    case `/api/issues/${ISSUE_ID}/children`: return { issues: [] };
    case `/api/issues/${ISSUE_ID}/pull-requests`: return { pull_requests: [] };
    default: return undefined;
  }
}
