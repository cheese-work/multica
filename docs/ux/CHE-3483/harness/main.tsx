import "@fontsource-variable/inter/opsz.css";
import "@fontsource-variable/geist-mono";
import "../../../../apps/desktop/src/renderer/src/globals.css";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "../../../../packages/core/i18n/react";
import { setApiInstance, ApiError } from "../../../../packages/core/api";
import { WorkspaceSlugProvider } from "../../../../packages/core/paths/index";
import { RESOURCES } from "../../../../packages/views/locales";
import { NavigationProvider } from "../../../../packages/views/navigation";
import { DecisionRequestCard } from "../../../../packages/views/chat/components/decision-request-card";

// One card per scenario. ?s=<scenario>&role=<owner|admin|member>&refuse=<code>&locale=<locale>
const query = new URLSearchParams(location.search);
const scenario = query.get("s") ?? "context";
const refuse = query.get("refuse");
const locale = (query.get("locale") ?? "en") as "en";

const hex = (c: string) => c.repeat(64);
const base = { field: "workspace.context", target_id: "ws-capture", keys: [] as string[], diff_kind: "text" as const, before_sha256: hex("b"), after_sha256: hex("c"), sha256: hex("e") };
const changes: Record<string, { type: string; change: unknown }> = {
  context: {
    type: "workspace.context.set",
    change: {
      ...base,
      diff: [{ old_start: 4, new_start: 4, lines: [" ## Delivery", " Ship reversible work without asking.", "-Escalate for spend, deploy and publication.", "+Escalate for spend, production deploy, client-facing publication,", "+an irreversible action, or an open product-direction choice.", " "] }],
      after: "## Delivery\nShip reversible work without asking.\nEscalate for spend, production deploy, client-facing publication,\nan irreversible action, or an open product-direction choice.\n",
    },
  },
  instructions: {
    type: "agent.instructions.set",
    change: {
      ...base, field: "agent.instructions", target_id: "agent-1",
      diff: [
        { old_start: 1, new_start: 1, lines: [" You are the dev team author.", "-Merge after one review.", "+Merge only under Agent Merge (CHE-2008).", " Never skip the cross-lab review gate."] },
        { old_start: 40, new_start: 40, lines: [" ## Output", "-Post progress comments.", "+Post exactly one final comment per run.", "+Update the status summary at each handoff."] },
      ],
      after: { lines: ["You are the dev team author.", "Merge only under Agent Merge (CHE-2008).", "Never skip the cross-lab review gate."] },
    },
  },
  settings: {
    type: "workspace.settings.patch",
    change: {
      ...base, field: "workspace.settings", target_id: "ws-capture", keys: ["compact_prompt", "default_model"], diff_kind: "json" as const,
      diff: [{ old_start: 1, new_start: 1, lines: ["-  \"compact_prompt\": \"Summarise briefly.\"", "+  \"compact_prompt\": \"Summarise in five bullets.\"", "-  \"default_model\": \"sonnet\"", "+  \"default_model\": \"opus\""] }],
      after: { compact_prompt: "Summarise in five bullets.", default_model: "opus" },
    },
  },
  bad: { type: "workspace.context.set", change: { ...base, field: "members.roles" } },
};
const fixture = changes[scenario] ?? changes.context!;

const stub = {
  listDecisionArtifacts: async () => [],
  getDecisionActionOutcome: async () => ({ id: "action", state: "pending" }),
  challengeDecisionAction: async () => ({ challenge_id: "ch" }),
  approveDecisionAction: async () => {
    if (refuse) throw new ApiError(`${refuse}: refused by the server`, 409, "Conflict");
    return { id: "action", state: "executed" };
  },
};
setApiInstance(stub as never);

const card = {
  id: "request", issue_id: "issue", issue_identifier: "CHE-3348", issue_title: "Owner-passkey-approved workspace configuration changes",
  source_comment_id: "source", ask: "Approve this change to the live configuration?", asking_agent_name: "Terra",
  state: "open" as const, created_at: "2026-11-08T05:00:00Z",
  action: { id: "action", action_type: fixture.type, payload_sha256: "payload", config_change: fixture.change, owner_only: true },
};

const navigation = {
  push: () => {}, replace: () => {}, back: () => {}, pathname: "/capture/chat", searchParams: new URLSearchParams(), hash: "",
  openInNewTab: () => {}, getShareableUrl: (path: string) => path,
};

ReactDOM.createRoot(document.getElementById("root")!).render(
  <QueryClientProvider client={new QueryClient()}>
    <WorkspaceSlugProvider slug="capture">
      <I18nProvider locale={locale} resources={RESOURCES}>
        <main style={{ maxWidth: 540, margin: "0 auto", padding: 16 }}>
          <NavigationProvider value={navigation as never}><DecisionRequestCard card={card as never} /></NavigationProvider>
        </main>
      </I18nProvider>
    </WorkspaceSlugProvider>
  </QueryClientProvider>,
);
