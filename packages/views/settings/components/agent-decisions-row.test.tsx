import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../../test/i18n";
import { AgentDecisionsRow } from "./agent-decisions-row";

const mockToastError = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: { getAgentDecisionSetting: vi.fn(), setAgentDecisionsEnabled: vi.fn() },
}));
vi.mock("sonner", () => ({ toast: { error: mockToastError, success: vi.fn() } }));

const read = vi.mocked(api.getAgentDecisionSetting);
const write = vi.mocked(api.setAgentDecisionsEnabled);
const switchName = "Agent decisions";

function mount(client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  const view = renderWithI18n(
    <QueryClientProvider client={client}>
      <AgentDecisionsRow wsId="ws" />
    </QueryClientProvider>,
  );
  return { client, ...view };
}

beforeEach(() => {
  mockToastError.mockReset();
  read.mockReset().mockResolvedValue({ enabled: false, stopped: null });
  write.mockReset().mockResolvedValue({ enabled: true });
});

describe("AgentDecisionsRow", () => {
  it("shows the help line and the value the server reports", async () => {
    read.mockResolvedValue({ enabled: true, stopped: null });
    mount();

    await waitFor(() => {
      expect(screen.getByRole("switch", { name: switchName })).toBeChecked();
    });
    expect(
      screen.getByText(
        "Agents may decide allowlisted gates for you. Each decision shows in the daily brief with a 24 h veto.",
      ),
    ).toBeInTheDocument();
  });

  it("refetches on entry, so a CLI change made after the cache filled shows up", async () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    });
    const first = mount(client);
    expect(await screen.findByRole("switch", { name: switchName })).not.toBeChecked();
    first.unmount();

    read.mockResolvedValue({ enabled: true, stopped: null });
    mount(client);

    await waitFor(() => {
      expect(screen.getByRole("switch", { name: switchName })).toBeChecked();
    });
  });

  it("disables the switch while the value is unknown", async () => {
    read.mockResolvedValue(null);
    mount();

    await waitFor(() => expect(read).toHaveBeenCalled());
    expect(screen.getByRole("switch", { name: switchName })).toHaveAttribute("aria-disabled", "true");
  });

  it("turns decisions off through the setting route and refreshes the value", async () => {
    read.mockResolvedValueOnce({ enabled: true, stopped: null });
    write.mockResolvedValue({ enabled: false });
    read.mockResolvedValue({ enabled: false, stopped: null });
    const user = userEvent.setup();
    mount();

    await user.click(await screen.findByRole("switch", { name: switchName }));

    await waitFor(() => expect(write).toHaveBeenCalledWith(false));
    await waitFor(() => {
      expect(screen.getByRole("switch", { name: switchName })).not.toBeChecked();
    });
  });

  it("shows an error and keeps the switch where it was when the write fails", async () => {
    write.mockRejectedValue(new Error("boom"));
    const user = userEvent.setup();
    mount();

    await user.click(await screen.findByRole("switch", { name: switchName }));

    await waitFor(() => expect(mockToastError).toHaveBeenCalledWith("boom"));
    expect(screen.getByRole("switch", { name: switchName })).not.toBeChecked();
  });

  describe("after an automatic stop", () => {
    beforeEach(() => {
      read.mockResolvedValue({ enabled: false, stopped: { reason: "excluded category: spend" } });
    });

    it("shows the stop reason and writes only after the owner confirms", async () => {
      const user = userEvent.setup();
      mount();

      await user.click(await screen.findByRole("switch", { name: switchName }));

      expect(await screen.findByText(/excluded category: spend/)).toBeInTheDocument();
      expect(write).not.toHaveBeenCalled();

      await user.click(screen.getByRole("button", { name: "Turn on" }));
      await waitFor(() => expect(write).toHaveBeenCalledWith(true));
    });

    it("writes nothing when the owner cancels", async () => {
      const user = userEvent.setup();
      mount();

      await user.click(await screen.findByRole("switch", { name: switchName }));
      await user.click(await screen.findByRole("button", { name: "Cancel" }));

      expect(write).not.toHaveBeenCalled();
      expect(screen.getByRole("switch", { name: switchName })).not.toBeChecked();
    });
  });
});
