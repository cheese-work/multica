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
import { registerAuthStore } from "@multica/core/auth";
import { decisionArtifactKeys } from "@multica/core/issues/decision-artifacts";
import { workspaceListOptions } from "../../../packages/core/workspace/queries";
import { WorkspaceSlugProvider } from "../../../packages/core/paths/hooks";
import { NavigationProvider } from "../../../packages/views/navigation/context";
import { DecisionRequestCard } from "../../../packages/views/chat/components/decision-request-card";
import "./globals.css";

const WS = "ws-harness";
const NOW = "2026-11-13T09:00:00Z";

const open = {
  id: "rq-fid", issue_id: "iss-3461", issue_identifier: "CHE-3461", issue_title: "Sync v0.8.0",
  asking_agent_name: "x99-claude-gstack", state: "open", created_at: "2026-11-13T08:40:00Z",
};
// Fixture names are the ones a blocks map case names. Add a fixture here when a case needs one;
// a case with no fixture here fails `missing-render`.
export const FIXTURES: Record<string, Record<string, unknown>> = {
  "run-budget-top-up": {
    ...open,
    ask: "Reopen CHE-3461 so the v0.8.0 sync can finish?\n\nIf you do nothing, CHE-3461 stays paused and nothing else changes.",
    action: {
      id: "act-fid-1", action_type: "issue.run_budget.top_up", payload_sha256: "p", state: "pending",
      preview: { sha256: "a".repeat(64), fields: [{ label: "Runs used", current: "44 of 25", proposed: "0 of 25" }] },
    },
  },
};

const qc = new QueryClient({ defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: Infinity } } });
setApiInstance({
  listDecisionArtifacts: async () => [],
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
