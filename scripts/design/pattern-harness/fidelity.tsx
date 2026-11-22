// Fidelity harness entry (CHE-5793 step 2). ?fixture=<name> renders the real component with the record the
// lock state draws; check-fidelity screenshots nothing here, it reads the DOM.
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { create } from "zustand";
import { I18nProvider } from "../../../packages/core/i18n/provider"; // not the barrel: it pulls the auth store, which hangs headless Chrome
import { RESOURCES } from "@multica/views/locales";
import { TooltipProvider } from "@multica/ui/components/ui/tooltip";
import { setApiInstance } from "@multica/core/api";
import { configureShortcutPlatform } from "@multica/core/shortcuts";
import { registerAuthStore } from "@multica/core/auth";
import { decisionArtifactKeys } from "@multica/core/issues/decision-artifacts";
import { workspaceListOptions } from "../../../packages/core/workspace/queries";
import { WorkspaceSlugProvider } from "../../../packages/core/paths/hooks";
import { NavigationProvider } from "../../../packages/views/navigation/context";
import { DecisionRequestCard } from "../../../packages/views/chat/components/decision-request-card";
import "./globals.css";

// The lock writes its send key for macOS ("(⌘ + ⏎)"); the headless browser runs on Linux.
configureShortcutPlatform("macos");

const WS = "ws-harness";
const NOW = "2026-11-13T09:00:00Z";

const open = {
  id: "rq-fid", issue_id: "iss-3461", issue_identifier: "CHE-3461", issue_title: "Sync v0.8.0",
  asking_agent_name: "x99-claude-gstack", state: "open", created_at: "2026-11-13T08:40:00Z",
};
// Fixture names are the ones a blocks map case names. Add a fixture here when a case needs one;
// a case with no fixture here fails `missing-render`.
const ask = (q: string, tail: string) => `${q}\n\n${tail}`;
const budgetAction = {
  id: "act-fid-1", action_type: "issue.run_budget.top_up", payload_sha256: "p", state: "pending",
  preview: { sha256: "a".repeat(64), fields: [{ label: "Runs used", current: "44 of 25", proposed: "0 of 25" }] },
};
const budgetAsk = ask("Reopen CHE-3461 so the v0.8.0 sync can finish?", "If you do nothing, CHE-3461 stays paused and nothing else changes.");
const picked = { state: "picked_up", answered_at: "2026-11-13T08:41:00Z", run: { id: "t", agent_name: "Sol" } };
const decided = (card: Record<string, unknown>, action: Record<string, unknown>, extra: Record<string, unknown> = {}) => ({
  ...card, state: "answered", record_comment_id: "rec", answer: picked,
  action: { ...(card.action as object), preview: undefined, config_change: undefined, decided_at: "2026-11-11T06:41:00Z", decided_by_name: "Cheese", ...action }, ...extra,
});
const budgetOpen = { ...open, created_at: "2026-11-11T06:20:00Z", ask: budgetAsk, action: budgetAction };
const diffChange = (field: string, kind: "json" | "text", lines: string[], after: unknown, meta?: Record<string, unknown>) => ({
  field, target_id: "ws-1", keys: [] as string[], diff_kind: kind, diff: [{ old_start: 1, new_start: 1, lines }], after, meta,
  before_sha256: "b".repeat(64), after_sha256: "c".repeat(64), sha256: "e".repeat(64),
});
const configOpen = {
  ...open, created_at: "2026-11-11T17:39:00Z", id: "rq-cfg", issue_title: "Default workflow", ask: ask("Send \u201cAgent review\u201d to x99-codex-sol instead of x99-agy-gemini?", "If you do nothing, Default keeps x99-agy-gemini."),
  action: { id: "act-cfg", action_type: "workflow.steps.set", payload_sha256: "p", state: "pending", owner_only: true,
    config_change: diffChange("workflow.steps", "json", ["-  \"handler\": \"x99-agy-gemini\"", "+  \"handler\": \"x99-codex-sol\""], {}) },
};
const textOpen = {
  ...open, id: "rq-txt", issue_title: "Workspace context", ask: ask("Update the \u201cReview exception\u201d rule in Workspace context?", "If you do nothing, the old rule stays."),
  action: { id: "act-txt", action_type: "workspace.context.set", payload_sha256: "p", state: "pending", owner_only: true,
    config_change: diffChange("workspace.context", "text", ["-until 13 Nov 2026", "+until 14 Nov 2026"], "until 14 Nov 2026") },
};
const batchItem = (n: number, title: string, used: string) => ({
  action_id: `a${n}`, issue_id: `iss-${n}`, issue_identifier: `CHE-${n}`, issue_title: title, state: "pending", used,
});
const batch = {
  ...open, id: "rq-batch", issue_title: "Sync L6", ask: ask("Reopen 3 issues so the v0.8.0 sync can finish?", "If you do nothing, all 3 stay paused."),
  batch_id: "b_12", batch_items: [batchItem(3459, "Sync L4 \u00b7 merge upstream", "31"), batchItem(3460, "Sync L5 \u00b7 brand v3.1", "27"), batchItem(3461, "Sync L6 \u00b7 Inbox, ZCode", "44")],
};
export const FIXTURES: Record<string, Record<string, unknown>> = {
  "run-budget-top-up": budgetOpen,
  "status-change-backlog": {
    ...open, id: "rq-st", issue_identifier: "CHE-3470", ask: ask("Move CHE-3470 back to Backlog?", "If you do nothing, it stays In progress."),
    action: { id: "act-st", action_type: "issue.status.set", payload_sha256: "p", state: "pending",
      preview: { sha256: "a".repeat(64), fields: [{ label: "Status", current: "In progress", proposed: "Backlog" }] } },
  },
  "batch-run-budget": batch,
  "run-budget-approved": decided(budgetOpen, { state: "executed", effect: "applied", revocable: true }),
  "run-budget-rejected": decided(budgetOpen, { state: "rejected" }),
  "delegate-approved": decided({ ...budgetOpen, issue_identifier: "CHE-3459" }, { state: "executed", effect: "applied", revocable: true, decided_by_name: "Tofu", on_behalf_of_name: "Cheese", decided_at: "2026-11-11T06:44:00Z" }),
  "config-change-agent-disable": configOpen,
  "config-change-applied": decided(configOpen, { state: "executed", effect: "applied", revocable: true, decided_at: "2026-11-11T17:52:00Z" }),
  "config-change-not-applied": decided(configOpen, { state: "failed", effect: "not_applied", effect_reason: "apply_failed", decided_at: "2026-11-11T17:52:00Z" }),
  "config-change-revoked": decided(configOpen, { state: "executed", effect: "applied", decided_at: "2026-11-11T17:52:00Z", revoke: { at: "2026-11-12T02:21:00Z", by_name: "Cheese", reason: "Wrong handler" } }),
  "text-change-workflow": textOpen,
  "question-answered": {
    ...open, id: "rq-q", ask: ask("Which reviewer should sign PR 1379?", "Terra or Sol."), options: [{ label: "Terra" }, { label: "Sol" }],
    state: "answered", record_comment_id: "rec", answer: { ...picked, chosen_option_index: 0, option_label: "Terra" },
  },
  "expired-top-up": decided({ ...budgetOpen, created_at: "2026-11-11T02:00:00Z", issue_identifier: "CHE-3470" }, { state: "expired", effect: "expired", decided_at: "2026-11-12T02:00:00Z", decided_by_name: undefined }),
};

const qc = new QueryClient({ defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: Infinity } } });
setApiInstance({
  listDecisionArtifacts: async () => [],
  getDecisionAction: async (id: string) => ({ id, payload_sha256: `hash-${id}` }),
  getDecisionActionOutcome: async (id: string) => ({ id, state: "pending" }),
  listRecentRuns: async () => [],
} as never);
registerAuthStore(create(() => ({ user: { id: "u1", name: "Cheese", email: "c@example.com" }, isLoading: false })) as never);
qc.setQueryData(workspaceListOptions().queryKey, [{ id: WS, slug: "harness", name: "Harness", created_at: NOW, updated_at: NOW }]);
for (const f of Object.values(FIXTURES)) qc.setQueryData(decisionArtifactKeys.list(f.id as string), []);
const nav = { push() {}, replace() {}, back() {}, pathname: "/", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p: string) => p };

const name = new URLSearchParams(location.search).get("fixture") ?? "";
const fixture = FIXTURES[name];
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={{ en: RESOURCES.en }}>
        <WorkspaceSlugProvider slug="harness">
          <NavigationProvider value={nav as never}>
            <TooltipProvider>
              {fixture ? (
                <div data-fidelity-root className="max-w-[540px] p-3"><DecisionRequestCard card={fixture as never} /></div>
              ) : (
                <p data-fidelity-error>unknown fidelity fixture: {name}</p>
              )}
            </TooltipProvider>
          </NavigationProvider>
        </WorkspaceSlugProvider>
      </I18nProvider>
    </QueryClientProvider>
  </StrictMode>,
);
