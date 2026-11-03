/**
 * Fixture for the page-load baseline (CHE-2976). Synthetic and deterministic,
 * like the typing fixture it borrows ids from. Sizes are meant to look like a
 * working workspace, not a demo: a few hundred issues, a full inbox, a handful
 * of projects and chats.
 */
import * as fx from "./typing-under-live-runs";

export const ISSUE_COUNT = 300;
export const INBOX_COUNT = 60;
export const PROJECT_COUNT = 12;
export const CHAT_COUNT = 8;
export const CHAT_MESSAGES = 40;

const STATUSES = ["backlog", "todo", "in_progress", "in_review", "done"] as const;
const iso = (n: number) => new Date(Date.UTC(2026, 8, 1) + n * 60_000).toISOString();
const uuid = (prefix: string, n: number) =>
  `${prefix}${String(n).padStart(4, "0")}-5555-4555-8555-555555555555`.slice(0, 36);

export const issueTitle = (n: number) => `Perf issue ${n}: tighten the retry path`;
export const projectTitle = (n: number) => `Perf project ${n}`;
export const inboxTitle = (n: number) => `Perf inbox item ${n}`;
export const chatTitle = (n: number) => `Perf chat ${n}`;

export const projectIds = Array.from({ length: PROJECT_COUNT }, (_, i) => uuid("eeee", i));
export const chatIds = Array.from({ length: CHAT_COUNT }, (_, i) => uuid("ffff", i));
export const chatAgentId = uuid("bbbb", 0);

export function buildIssues() {
  return Array.from({ length: ISSUE_COUNT }, (_, i) => ({
    id: uuid("1111", i), workspace_id: fx.WORKSPACE_ID, number: i + 2,
    identifier: `PERF-${i + 2}`, title: issueTitle(i + 2),
    description: "Synthetic issue for the page-load baseline.",
    status: STATUSES[i % STATUSES.length]!, status_category: "in_progress",
    status_name: "", priority: (["none", "low", "medium", "high"] as const)[i % 4]!,
    assignee_type: null, assignee_id: null, creator_type: "user", creator_id: fx.USER_ID,
    parent_issue_id: null, project_id: projectIds[i % PROJECT_COUNT]!, position: i,
    start_date: null, due_date: null, stage: null, metadata: {}, properties: {},
    labels: [], reactions: [], created_at: iso(i), updated_at: iso(i),
  }));
}

export function buildInbox() {
  return Array.from({ length: INBOX_COUNT }, (_, i) => ({
    id: uuid("2222", i), workspace_id: fx.WORKSPACE_ID, recipient_type: "member",
    recipient_id: fx.USER_ID, actor_type: "system", actor_id: null,
    type: "new_comment", severity: "info", issue_id: fx.ISSUE_ID,
    title: inboxTitle(i), body: "A synthetic comment arrived.",
    issue_status: "in_progress", read: i % 3 === 0, archived: false,
    created_at: iso(i), updated_at: iso(i),
  }));
}

export function buildProjects() {
  return projectIds.map((id, i) => ({
    id, workspace_id: fx.WORKSPACE_ID, title: projectTitle(i), description: null,
    icon: null, status: "in_progress", priority: "none", lead_type: null, lead_id: null,
    start_date: null, due_date: null, created_at: iso(i), updated_at: iso(i),
    issue_count: ISSUE_COUNT / PROJECT_COUNT, done_count: 5, resource_count: 0,
  }));
}

export function buildChatSessions() {
  return chatIds.map((id, i) => ({
    id, workspace_id: fx.WORKSPACE_ID, agent_id: chatAgentId, creator_id: fx.USER_ID,
    title: chatTitle(i), status: "active", has_unread: false, created_at: iso(i),
    updated_at: iso(i),
  }));
}

export function buildChatMessages(sessionId: string) {
  return Array.from({ length: CHAT_MESSAGES }, (_, i) => ({
    id: uuid("3333", i), chat_session_id: sessionId,
    role: i % 2 ? "assistant" : "user", task_id: null, created_at: iso(i),
    content: `Message ${i}. ${"The retry path backs off, then gives up. ".repeat(6)}`,
  }));
}

export const agent = {
  id: chatAgentId, workspace_id: fx.WORKSPACE_ID, runtime_id: uuid("dddd", 0),
  name: "Perf Agent", description: "", instructions: "", avatar_url: null,
  runtime_mode: "local", runtime_config: {}, custom_env: {}, custom_args: [],
  visibility: "workspace", status: "idle", max_concurrent_tasks: 1,
  owner_id: fx.USER_ID, skills: [], created_at: iso(0), updated_at: iso(0),
  archived_at: null, archived_by: null,
};
