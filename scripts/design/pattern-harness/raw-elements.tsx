// S0b raw-element pages (CHE-5534, S4b-SS1): the real component, seeded with query data, one page per file.
// Not a pattern: these exist so scripts/design/capture-frame.mjs can screenshot files the S4b audit lists.
import { useEffect, useRef, type ReactNode } from "react";
import type { QueryClient } from "@tanstack/react-query";
import { create } from "zustand";
import { setApiInstance } from "@multica/core/api";
import { registerAuthStore } from "@multica/core/auth";
import { issueDetailOptions, childIssuesOptions } from "@multica/core/issues/queries";
import { issueRelationsOptions } from "@multica/core/issues/relations";
import { issueStatusListOptions } from "@multica/core/issue-statuses/queries";
import { openDecisionRequestsOptions, issueDecisionCardsOptions } from "@multica/core/chat/queries";
import { agentListOptions, memberListOptions, workspaceListOptions } from "@multica/core/workspace/queries";
import { decisionArtifactKeys } from "@multica/core/issues/decision-artifacts";
import { labelListOptions } from "@multica/core/labels/queries";
import { propertyListOptions } from "@multica/core/properties/queries";
import { BUILT_IN_STATUS_CATEGORY, BUILT_IN_STATUS_LABEL, BUILT_IN_STATUS_ORDER } from "@multica/core/issues/config";
import { IssueDecisionBanner } from "../../../packages/views/issues/components/issue-decision-banner";
import { IssueHierarchy } from "../../../packages/views/issues/components/hierarchy/hierarchy-minimap";
import { HierarchyDialog } from "../../../packages/views/issues/components/hierarchy/hierarchy-dialog";
import { WakeupEventRow } from "../../../packages/views/chat/components/wakeup-event-row";
import { ConditionParams } from "../../../packages/views/issues/components/wakeup-condition-params";
import { emptyWakeupDraft, type WakeupCondition, type WakeupDraft } from "../../../packages/views/issues/components/wakeup-draft";

const WS = "ws-harness";
const NOW = "2026-11-13T09:00:00Z";
const AGENT = "agent-design";
const issue = (number: number, status: string, title: string, extra: Record<string, unknown> = {}) => ({
  id: `iss-${number}`, workspace_id: WS, number, identifier: `CHE-${number}`, title, description: null, status,
  priority: "medium", assignee_type: "agent", assignee_id: AGENT, creator_type: "member", creator_id: "u1",
  parent_issue_id: null, created_at: NOW, updated_at: NOW, ...extra,
});

const PARENT = issue(5430, "in_progress", "S4b: capture raw-element files in doop");
const CURRENT = issue(5534, "in_progress", "Capture 5 S0b raw-element screenshots, batch SS1 of 7", { parent_issue_id: PARENT.id, stage: 1 });
const SIBLINGS = [
  issue(5531, "done", "Capture batch SS0: harness and reference frame", { parent_issue_id: PARENT.id, stage: 1 }),
  issue(5535, "todo", "Capture 5 S0b raw-element screenshots, batch SS2 of 7", { parent_issue_id: PARENT.id, stage: 2 }),
  issue(5536, "backlog", "Capture 5 S0b raw-element screenshots, batch SS3 of 7", { parent_issue_id: PARENT.id, stage: 2 }),
  issue(5537, "backlog", "Capture 5 S0b raw-element screenshots, batch SS4 of 7", { parent_issue_id: PARENT.id, stage: 3 }),
];
const CHILDREN = [
  issue(5541, "done", "Add harness pages for the 5 files", { parent_issue_id: CURRENT.id, stage: 1 }),
  issue(5542, "in_progress", "Upload captures and build the reference frame", { parent_issue_id: CURRENT.id, stage: 2 }),
  issue(5543, "todo", "Mark files captured in ui-drift.md", { parent_issue_id: CURRENT.id, stage: 2 }),
];

const statusEntries = BUILT_IN_STATUS_ORDER.map((key, position) => ({
  id: `st-${key}`, workspace_id: WS, key, name: BUILT_IN_STATUS_LABEL[key], description: "",
  category: BUILT_IN_STATUS_CATEGORY[key], color: "#888888", icon: null, is_system: true, position,
  archived_at: null, created_at: NOW, updated_at: NOW,
}));

const wakeup = (n: number, title: string, instruction: string) => ({
  id: `wk-${n}`, agent_id: AGENT, title, instruction, fire_at: "2026-11-13T11:30:00Z", created_at: "2026-11-13T09:05:00Z",
});

const card = (id: string, issueNumber: number, state: "open" | "answered", extra: Record<string, unknown> = {}) => ({
  id, issue_id: `iss-${issueNumber}`, issue_identifier: `CHE-${issueNumber}`, issue_title: "Capture 5 S0b raw-element screenshots",
  ask: "Which reference frame layout should the 7 batches share?", asking_agent_name: "x99-claude-sonnet-design",
  options: [
    { label: "One frame per batch", what_happens: "Each batch adds a frame; 7 frames total." },
    { label: "One frame, grows", what_happens: "Every batch appends to the same frame." },
  ],
  recommended_option_index: 1, state, created_at: "2026-11-13T08:40:00Z", ...extra,
});

// childIssuesOptions refetches on every mount; answer from the fixtures so no request leaves the page.
const CHILDREN_OF: Record<string, unknown[]> = { [PARENT.id]: [CURRENT, ...SIBLINGS], [CURRENT.id]: CHILDREN };

export function seedRawElements(qc: QueryClient) {
  setApiInstance({
    listChildIssues: async (id: string) => ({ issues: CHILDREN_OF[id] ?? [] }),
    getIssue: async (id: string) => (id === PARENT.id ? PARENT : CURRENT),
    listDecisionArtifacts: async () => [],
  } as never);
  // Components read the signed-in user (assignee pickers, decision card); no real auth runs here.
  registerAuthStore(create(() => ({ user: { id: "u1", name: "Cheese", email: "c@example.com" }, isLoading: false })) as never);
  const set = (key: readonly unknown[], data: unknown) => qc.setQueryData(key, data);
  // useWorkspaceId resolves the slug "harness" through the workspace list.
  set(workspaceListOptions().queryKey, [{ id: WS, slug: "harness", name: "Harness", created_at: NOW, updated_at: NOW }]);
  set(agentListOptions(WS).queryKey, [{ id: AGENT, workspace_id: WS, name: "x99-claude-sonnet-design", runtime_id: "rt1", archived_at: null }]);
  set(memberListOptions(WS).queryKey, [{ id: "m1", user_id: "u1", workspace_id: WS, name: "Cheese", email: "c@example.com", role: "owner" }]);
  set(issueStatusListOptions(WS).queryKey, { statuses: statusEntries, categories: [], total: statusEntries.length });
  set(labelListOptions(WS).queryKey, { labels: [{ id: "lb1", name: "design-system", color: "#3b82f6" }, { id: "lb2", name: "needs-review", color: "#f59e0b" }] });
  set(propertyListOptions(WS).queryKey, {
    properties: [
      { id: "pr1", name: "Design", type: "url", archived: false, config: {} },
      { id: "pr2", name: "Readiness", type: "select", archived: false, config: { options: [{ id: "o1", name: "ready" }, { id: "o2", name: "gated-human" }] } },
    ],
  });

  // Hierarchy: current has a parent, three siblings and three children.
  set(issueDetailOptions(WS, PARENT.id).queryKey, PARENT);
  set(childIssuesOptions(WS, PARENT.id).queryKey, [CURRENT, ...SIBLINGS]);
  set(childIssuesOptions(WS, CURRENT.id).queryKey, CHILDREN);
  set(issueRelationsOptions(WS, CURRENT.id, 1).queryKey, {
    nodes: [...CHILDREN, PARENT].map(({ id, identifier, title, status }) => ({ id, identifier, title, status })),
    edges: [{ source: CURRENT.id, target: "iss-5541", type: "wakeup" }],
    omitted_count: 0, depth: 1,
  });

  // Decision banner: 5701 has an open card; 5702 has an answered one (folds).
  set(openDecisionRequestsOptions(WS).queryKey, [{ ...card("rq-open", 5701, "open"), asking_agent_id: AGENT, escalation_class: "product_direction", chat_session_id: "cs1", message_id: null }]);
  set(issueDecisionCardsOptions(WS, "iss-5701").queryKey, []);
  set(issueDecisionCardsOptions(WS, "iss-5702").queryKey, [card("rq-used", 5702, "answered", {
    answer: { state: "delivered", text: "One frame, grows", option: "One frame, grows" }, record_comment_id: "cm1",
  })]);
  for (const id of ["rq-open", "rq-used"]) set(decisionArtifactKeys.list(id), []);
}

const Label = ({ children }: { children: ReactNode }) => <p className="mb-1 mt-4 text-caption text-muted-foreground first:mt-0">{children}</p>;

/** Runs `pick` once after mount, e.g. to click a toggle so the expanded state is captured. */
function After({ pick, children }: { pick: (root: HTMLElement) => HTMLElement | null | undefined; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => { pick(ref.current!)?.click(); }, [pick]);
  return <div ref={ref}>{children}</div>;
}

const noop = () => {};

export function DecisionBanner() {
  return (
    <div className="max-w-[760px] p-6">
      <Label>need_decision, one open card</Label>
      <IssueDecisionBanner issueId="iss-5701" status="need_decision" ownerAgentId={AGENT} />
      <Label>answered card folds (decision used)</Label>
      <IssueDecisionBanner issueId="iss-5702" status="in_progress" ownerAgentId={AGENT} />
    </div>
  );
}

export function HierarchyMinimap() {
  return (
    <div className="p-6">
      <div className="w-[320px] rounded-xl border border-surface-border p-3">
        <IssueHierarchy issue={CURRENT as never} />
      </div>
    </div>
  );
}

export function HierarchyDialogPage() {
  const heading = useRef<HTMLHeadingElement>(null);
  return (
    <div className="p-6">
      <h2 ref={heading} className="text-label">Hierarchy</h2>
      <HierarchyDialog issue={CURRENT as never} headingRef={heading} onClose={noop} />
    </div>
  );
}

export function WakeupEventRowPage() {
  const expand = (root: HTMLElement) => root.querySelector<HTMLElement>("[aria-expanded=false]");
  return (
    <div className="max-w-[560px] p-6">
      <Label>collapsed, titled</Label>
      <WakeupEventRow timeZone="Asia/Bangkok" wakeup={wakeup(1, "Check CI on the capture PR", "Check the PR checks for CHE-5534.\nIf green, mark the PR ready.")} />
      <Label>expanded, untitled, issue references chip once</Label>
      <After pick={expand}>
        <WakeupEventRow timeZone="Asia/Bangkok" wakeup={wakeup(2, "", "Re-run capture for CHE-5534 and CHE-5430 when CHE-5420 changes; CHE-5534 again is plain text.")} />
      </After>
    </div>
  );
}

const draft = (condition: WakeupCondition, patch: Partial<WakeupDraft> = {}): WakeupDraft => ({
  ...emptyWakeupDraft(AGENT, "Asia/Bangkok", new Date(NOW)), condition, ...patch,
});
const scope = { workspaceId: WS, issueId: CURRENT.id };

export function ConditionParamsPage() {
  const rows: [string, WakeupDraft][] = [
    ["field: status", draft("field", { field: "status", fieldTarget: "in_review" })],
    ["field: property select", draft("field", { field: "property", fieldTarget: "pr2", fieldValue: "o2" })],
    ["field: label", draft("field", { field: "label", fieldTarget: "lb1" })],
    ["children (stage)", draft("children", { stage: 1 })],
    ["pull_request", draft("pull_request", { prEvent: "merged" })],
    ["other_issue", draft("other_issue", { otherIssue: { id: "iss-5420", identifier: "CHE-5420" }, otherState: "in_review" })],
    ["at", draft("at", { atPreset: "tomorrow" })],
    ["recurring", draft("recurring", { recurrence: "weekdays" })],
    ["run_end (any agent)", draft("run_end")],
    ["custom (events)", draft("custom", { events: ["task.completed", "comment.created"] })],
  ];
  return (
    <div className="max-w-[720px] p-6">
      {rows.map(([label, d]) => (
        <div key={label} className="flex flex-wrap items-center gap-2 py-1.5">
          <span className="w-44 shrink-0 text-caption text-muted-foreground">{label}</span>
          <ConditionParams draft={d} scope={scope} update={noop} />
        </div>
      ))}
    </div>
  );
}
