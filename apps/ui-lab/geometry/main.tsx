// Real-browser harness for bare issue keys (CHE-1829): production RichContent,
// IssueMentionCard, IssueChip, hooks, queries, providers and Tailwind styles.
// Only API data is substituted. `getIssue` stays pending until the driver calls
// `window.__release()`, so the pending box and the resolved chip can be measured
// in the same page.
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@fontsource-variable/inter";
import "../src/styles.css";
import { ApiClient, setApiInstance } from "@multica/core/api";
import { I18nProvider } from "@multica/core/i18n/react";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { setCurrentWorkspace } from "@multica/core/platform";
import type { Issue, Workspace } from "@multica/core/types";
import { NavigationProvider, type NavigationAdapter } from "@multica/views/navigation";
import { RESOURCES } from "@multica/views/locales";
import { RichContent } from "../../../packages/views/rich-content";
import { TooltipProvider } from "@multica/ui/components/ui/tooltip";

declare global {
  interface Window {
    __release: () => void;
    __getIssueCalls: string[];
  }
}

const params = new URLSearchParams(location.search);
const title = params.get("title") ?? "fix(db): resolve duplicate migration prefix";
const surface = params.get("surface") ?? "chat";
const outcome = params.get("outcome") ?? "found";
const content = params.get("content") ?? "see MUL-7 now and after.";

const time = "2026-10-17T00:00:00Z";
const workspace: Workspace = {
  id: "10000000-0000-4000-8000-000000000001", slug: "probe", name: "Probe",
  description: "", context: null, settings: {}, repos: [], issue_prefix: "MUL",
  avatar_url: null, created_at: time, updated_at: time,
};
const issue = {
  id: "20000000-0000-4000-8000-000000000007", workspace_id: workspace.id, number: 7,
  identifier: "MUL-7", title, description: null, status: "todo", priority: "none",
  assignee_type: null, assignee_id: null, creator_type: "agent", creator_id: "a",
  parent_issue_id: null, project_id: null, position: 0, stage: null, start_date: null,
  due_date: null, metadata: {}, properties: {}, created_at: time, updated_at: time,
} as unknown as Issue;

let release: () => void = () => {};
const held = new Promise<void>((resolve) => { release = resolve; });
window.__release = () => release();
window.__getIssueCalls = [];

class ProbeApi extends ApiClient {
  async getIssue(id: string): Promise<Issue> {
    window.__getIssueCalls.push(id);
    await held;
    if (outcome === "missing") throw new (await import("@multica/core/api")).ApiError("Not found", 404, "Not Found");
    return issue;
  }
  async listIssues() { return { issues: [], total: 0 } as never; }
}
setApiInstance(new ProbeApi("http://127.0.0.1:1"));
setCurrentWorkspace(workspace.slug, workspace.id);

const client = new QueryClient({
  defaultOptions: { queries: { retry: false, staleTime: Infinity, refetchOnWindowFocus: false } },
});
client.setQueryData(workspaceKeys.list(), [workspace]);

const navigation: NavigationAdapter = {
  push() {}, replace() {}, back() {}, pathname: "/probe/issues", hash: "",
  searchParams: new URLSearchParams(), getShareableUrl: (p: string) => `${location.origin}${p}`,
} as NavigationAdapter;

// Class sets follow the real hosts: chat bubble text-sm, description text-base.
const hostClass = surface === "chat" ? "text-sm leading-relaxed" : "text-base leading-normal";

createRoot(document.getElementById("root")!).render(
  <QueryClientProvider client={client}>
    <I18nProvider locale="en" resources={{ en: RESOURCES.en }}>
      <WorkspaceSlugProvider slug={workspace.slug}>
        <NavigationProvider value={navigation}>
          <TooltipProvider>
            <div style={{ padding: 12, maxWidth: 560 }}>
              <div id="host" className={hostClass}>
                <RichContent content={content} phase="settled" />
              </div>
            </div>
          </TooltipProvider>
        </NavigationProvider>
      </WorkspaceSlugProvider>
    </I18nProvider>
  </QueryClientProvider>,
);
