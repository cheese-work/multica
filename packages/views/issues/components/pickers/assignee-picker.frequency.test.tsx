// The frequency ranking only sorts rows inside the open popover. A closed
// picker (one per property row on an issue) must not pay for that request.
import { describe, expect, it, vi } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import enIssues from "../../../locales/en/issues.json";
import { AssigneePicker } from "./assignee-picker";

const fetchFrequency = vi.fn(async () => []);

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/auth", () => ({ useAuthStore: () => ({ id: "user-1" }) }));
vi.mock("@multica/core/agents", () => ({ isAgentRuntimeBound: () => true }));
vi.mock("@multica/core/permissions", () => ({
  canAssignAgentToIssue: () => ({ allowed: true }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Ada Lovelace" }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"], queryFn: async () => [] }),
  agentListOptions: () => ({ queryKey: ["agents"], queryFn: async () => [] }),
  squadListOptions: () => ({ queryKey: ["squads"], queryFn: async () => [] }),
  assigneeFrequencyOptions: () => ({ queryKey: ["frequency"], queryFn: fetchFrequency }),
}));
vi.mock("../../../common/actor-avatar", () => ({
  ActorAvatar: () => <span data-testid="actor-avatar" />,
}));

function renderPicker(open: boolean) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={{ en: { issues: enIssues } }}>
        <AssigneePicker
          assigneeType={null}
          assigneeId={null}
          onUpdate={() => {}}
          open={open}
          onOpenChange={() => {}}
        />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("AssigneePicker frequency ranking", () => {
  it("does not request it while the picker is closed", async () => {
    fetchFrequency.mockClear();
    renderPicker(false);
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(fetchFrequency).not.toHaveBeenCalled();
  });

  it("requests it once the picker opens", async () => {
    fetchFrequency.mockClear();
    renderPicker(true);
    await waitFor(() => expect(fetchFrequency).toHaveBeenCalledTimes(1));
  });
});
