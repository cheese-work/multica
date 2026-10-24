import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatWakeup } from "@multica/core/types";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import enChat from "../../locales/en/chat.json";
import { ChatFollowUpsPanel } from "./chat-followups-panel";

const mockApi = vi.hoisted(() => ({
  listChatWakeups: vi.fn(),
  cancelChatWakeup: vi.fn(),
}));
vi.mock("@multica/core/api", () => ({ api: mockApi }));

const TEST_RESOURCES = { en: { chat: enChat } };

const wakeup = (id: string, chat_session_id = "s1"): ChatWakeup => ({
  id,
  chat_session_id,
  instruction: `Instruction ${id}`,
  fire_at: "2026-10-21T04:40:00Z",
  status: "pending",
  task_id: null,
  condition: null,
});

function renderPanel(sessionId: string | null) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatFollowUpsPanel sessionId={sessionId} agentName="Ada" />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("ChatFollowUpsPanel", () => {
  beforeEach(() => {
    mockApi.listChatWakeups.mockReset();
    mockApi.cancelChatWakeup.mockReset();
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders nothing and loads nothing without a session", () => {
    const { container } = renderPanel(null);
    expect(container).toBeEmptyDOMElement();
    expect(mockApi.listChatWakeups).not.toHaveBeenCalled();
  });

  it("renders nothing when the DM has no pending follow-up", async () => {
    mockApi.listChatWakeups.mockResolvedValue({ wakeups: [] });
    const { container } = renderPanel("s1");
    await waitFor(() => expect(mockApi.listChatWakeups).toHaveBeenCalledWith("s1"));
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("does not flash a loading strip for a fast load, but shows one for a slow load", async () => {
    mockApi.listChatWakeups.mockReturnValue(new Promise(() => {}));
    const { container } = renderPanel("s1");
    expect(container).toBeEmptyDOMElement();
    expect(await screen.findByText("Loading scheduled follow-ups…")).toBeInTheDocument();
  });

  it("lists the follow-ups of the DM, including one inherited from an earlier DM", async () => {
    mockApi.listChatWakeups.mockResolvedValue({ wakeups: [wakeup("w1"), wakeup("w2", "s0")] });
    renderPanel("s1");
    expect(await screen.findByRole("region", { name: "Scheduled follow-ups (2)" })).toBeInTheDocument();
    expect(screen.getByText("Instruction w2")).toBeInTheDocument();
    expect(screen.getByText(/Set in the previous DM\./)).toBeInTheDocument();
  });

  it("cancels, then refreshes the list from the server", async () => {
    mockApi.listChatWakeups
      .mockResolvedValueOnce({ wakeups: [wakeup("w1")] })
      .mockResolvedValue({ wakeups: [] });
    mockApi.cancelChatWakeup.mockResolvedValue(undefined);
    renderPanel("s1");
    fireEvent.click(await screen.findByRole("button", { name: /^Cancel follow-up/ }));
    await waitFor(() => expect(mockApi.cancelChatWakeup).toHaveBeenCalledWith("s1", "w1"));
    await waitFor(() => expect(screen.queryByText("Instruction w1")).not.toBeInTheDocument());
    expect(mockApi.listChatWakeups).toHaveBeenCalledTimes(2);
  });

  it("cancels an inherited follow-up through the DM that owns it", async () => {
    mockApi.listChatWakeups.mockResolvedValue({ wakeups: [wakeup("w2", "s0")] });
    mockApi.cancelChatWakeup.mockResolvedValue(undefined);
    renderPanel("s1");
    fireEvent.click(await screen.findByRole("button", { name: /^Cancel follow-up/ }));
    await waitFor(() => expect(mockApi.cancelChatWakeup).toHaveBeenCalledWith("s1", "w2"));
  });

  it("keeps the follow-up and offers a retry when the cancel fails", async () => {
    mockApi.listChatWakeups.mockResolvedValue({ wakeups: [wakeup("w1")] });
    mockApi.cancelChatWakeup.mockRejectedValueOnce(new Error("boom")).mockResolvedValue(undefined);
    renderPanel("s1");
    fireEvent.click(await screen.findByRole("button", { name: /^Cancel follow-up/ }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Could not cancel the follow-up. It may still run.");
    expect(screen.getByText("Instruction w1")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(mockApi.cancelChatWakeup).toHaveBeenCalledTimes(2));
  });

  it("shows a retryable error when the list cannot be loaded", async () => {
    mockApi.listChatWakeups.mockRejectedValueOnce(new Error("down")).mockResolvedValue({ wakeups: [wakeup("w1")] });
    renderPanel("s1");
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load Ada's scheduled follow-ups.");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Instruction w1")).toBeInTheDocument();
  });

  it("disables Cancel while the browser is offline", async () => {
    vi.spyOn(navigator, "onLine", "get").mockReturnValue(false);
    mockApi.listChatWakeups.mockResolvedValue({ wakeups: [wakeup("w1")] });
    renderPanel("s1");
    expect(await screen.findByRole("button", { name: /^Cancel follow-up/ })).toBeDisabled();
    expect(screen.getByRole("status")).toHaveTextContent("Offline.");
  });
});
