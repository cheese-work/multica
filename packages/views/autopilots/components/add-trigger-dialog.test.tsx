import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useAutopilotTriggerDraftStore } from "@multica/core/autopilots/stores";
import { renderWithI18n } from "../../test/i18n";

// CHE-3523: the add-trigger dialog's unsent kind and label live in the keyed
// draft store, so they survive a route change and clear only after a save.

const mockCreateTrigger = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-test" }));
vi.mock("@multica/core/autopilots/queries", () => ({
  cronPreviewOptions: (wsId: string, expr: string, tz: string) => ({
    queryKey: ["cron-preview", wsId, expr, tz],
    queryFn: async () => ({ next_runs: ["2126-07-14T01:00:00Z"] }),
    retry: false,
  }),
}));
vi.mock("@multica/core/autopilots/mutations", () => ({
  useCreateAutopilotTrigger: () => ({ mutateAsync: mockCreateTrigger }),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { AddTriggerDialog } from "./add-trigger-dialog";

function renderDialog(autopilotId = "ap-1") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <AddTriggerDialog open onOpenChange={vi.fn()} autopilotId={autopilotId} />
    </QueryClientProvider>,
  );
}

const labelInput = () => screen.getByPlaceholderText("e.g. Weekday morning");

describe("AddTriggerDialog draft persistence", () => {
  beforeEach(() => {
    useAutopilotTriggerDraftStore.setState({ drafts: {} });
    mockCreateTrigger.mockReset();
  });

  it("restores the trigger kind and label after an unmount and remount, per autopilot", async () => {
    const user = userEvent.setup();
    const first = renderDialog();
    await user.click(screen.getByRole("button", { name: /Webhook/ }));
    await user.type(labelInput(), "Nightly hook");
    first.unmount();

    const second = renderDialog();
    expect(labelInput()).toHaveValue("Nightly hook");
    expect(screen.getByText(/generate a webhook URL/)).toBeInTheDocument();
    second.unmount();

    renderDialog("ap-2");
    expect(labelInput()).toHaveValue("");
  });

  it("keeps the draft when the add fails and clears it after a successful add", async () => {
    const user = userEvent.setup();
    mockCreateTrigger.mockRejectedValueOnce(new Error("nope"));
    const first = renderDialog();
    await user.click(screen.getByRole("button", { name: /Webhook/ }));
    await user.type(labelInput(), "Nightly hook");
    await user.click(screen.getByRole("button", { name: "Add trigger" }));
    await waitFor(() => expect(mockCreateTrigger).toHaveBeenCalledTimes(1));
    first.unmount();

    mockCreateTrigger.mockResolvedValueOnce({ id: "tr-1" });
    const second = renderDialog();
    expect(labelInput()).toHaveValue("Nightly hook");
    await user.click(screen.getByRole("button", { name: "Add trigger" }));
    await waitFor(() => expect(mockCreateTrigger).toHaveBeenCalledTimes(2));
    second.unmount();

    renderDialog();
    expect(labelInput()).toHaveValue("");
  });
});
