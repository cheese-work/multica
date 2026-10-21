import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it, vi } from "vitest";

import { renderWithI18n } from "../../test/i18n";

// CHE-1853 review finding (P1): a failed save leaves the dialog open, and the
// mutation that carried the key as its variable must not stay in the
// MutationCache while the admin looks at the error. The dialog keeps the typed
// key so the admin can retry.
const fake = vi.hoisted(() => {
  class FakeApiError extends Error {
    readonly status: number;
    readonly body?: unknown;
    constructor(message: string, status: number, body?: unknown) {
      super(message);
      this.name = "ApiError";
      this.status = status;
      this.body = body;
    }
  }
  return { FakeApiError, putJevCredential: vi.fn(), deleteJevCredential: vi.fn() };
});
vi.mock("@multica/core/api", () => ({
  ApiError: fake.FakeApiError,
  api: { putJevCredential: fake.putJevCredential, deleteJevCredential: fake.deleteJevCredential },
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

import { JevCredentialPane } from "./jev-credential-pane";

const SECRET = "sk-live-0123456789abcdef";

describe("Jev credential key dialog after a failed save", () => {
  it("releases the key from the mutation cache while keeping it in the field for a retry", async () => {
    fake.putJevCredential
      .mockReset()
      .mockRejectedValueOnce(new fake.FakeApiError("failed", 503, { problem: "configuration_unavailable" }))
      .mockResolvedValueOnce({ present: true });
    const onNotice = vi.fn();
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    renderWithI18n(
      <QueryClientProvider client={client}>
        <JevCredentialPane
          credential={{ data: { present: false, configurable: true }, isError: false } as never}
          config={{ version: 1, routing_mode: "off", min_confidence: null, api_url: "", history: [] }}
          canManage
          online
          onNotice={onNotice}
        />
      </QueryClientProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    const dialog = await screen.findByRole("dialog", { name: "Set Jev key" });
    fireEvent.change(within(dialog).getByLabelText("API key"), { target: { value: SECRET } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save key" }));

    expect(await within(dialog).findByRole("alert")).toHaveTextContent("Couldn't save the key.");
    const held = JSON.stringify(client.getMutationCache().getAll().map((mutation) => mutation.state.variables));
    expect(held).not.toContain(SECRET);

    // Retry state is intact: same field value, Save key enabled, second attempt succeeds.
    expect(within(dialog).getByLabelText("API key")).toHaveValue(SECRET);
    const save = within(dialog).getByRole("button", { name: "Save key" });
    expect(save).toBeEnabled();
    fireEvent.click(save);
    await waitFor(() => expect(onNotice).toHaveBeenCalledWith("key_saved"));
    expect(fake.putJevCredential).toHaveBeenCalledTimes(2);
    expect(fake.putJevCredential).toHaveBeenLastCalledWith("ws-1", SECRET);
  });
});
