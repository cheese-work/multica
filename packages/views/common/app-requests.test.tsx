import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, expect, it, vi } from "vitest";
import settings from "../locales/en/settings.json";

const f = vi.hoisted(() => ({
  requests: [{
    id: "request-1", kind: "action", status: "pending", approver_user_id: "viewer",
    agent_id: "agent", toolkit_slug: "gmail", tool_slug: "GMAIL_SEND_EMAIL",
    account_scope: "personal", action_class: "send", arguments: { to: "original@example.com" },
    expires_at: "2030-01-01T00:00:00Z",
  }],
  mutate: vi.fn(), mutateAsync: vi.fn().mockResolvedValue({}),
}));
vi.mock("@multica/core/api", () => ({ ApiError: class extends Error {}, api: {} }));
vi.mock("@multica/core/auth", () => ({ useAuthStore: (select: any) => select({ user: { id: "viewer" } }) }));
vi.mock("@multica/core/config", () => ({ useFeatureEnabled: () => true }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws" }));
vi.mock("@multica/core/workspace/hooks", () => ({ useActorName: () => ({ getActorName: () => "Actor" }) }));
vi.mock("@multica/core/connected-apps", () => ({
  chatAppRequestsOptions: () => ({ queryKey: ["requests"], queryFn: async () => f.requests, initialData: f.requests }),
  connectedAppsKeys: { all: () => ["apps"] }, issueAppRequestsOptions: () => ({}),
  useDecideAppRequest: () => ({ mutate: f.mutate, mutateAsync: f.mutateAsync, isPending: false }),
}));
vi.mock("../i18n", () => ({ useLocale: () => "en", useT: () => ({ t: (select: any) => select(settings) }) }));
vi.mock("../navigation", () => ({ currentPath: () => "/acme/chat", useNavigation: () => ({ pathname: "/acme/chat", hash: "", searchParams: new URLSearchParams(), replace: vi.fn() }) }));
vi.mock("@multica/ui/components/ui/button", () => ({ Button: ({ children, size, variant, ...props }: any) => <button {...props}>{children}</button> }));
vi.mock("@multica/ui/components/ui/badge", () => ({ Badge: ({ children }: any) => <span>{children}</span> }));
vi.mock("@multica/ui/components/ui/input", () => ({ Input: (props: any) => <input {...props} /> }));
vi.mock("@multica/ui/components/ui/textarea", () => ({ Textarea: (props: any) => <textarea {...props} /> }));

import { ChatAppRequests } from "./app-requests";
import { useAppRequestDraftStore } from "@multica/core/connected-apps/request-draft-store";

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ChatAppRequests sessionId="chat" /></QueryClientProvider>);
}
afterEach(() => { cleanup(); vi.clearAllMocks(); f.requests.splice(1); useAppRequestDraftStore.setState({ drafts: {} }); });

// Regression tests for the CHE-5608 review of PR 1645: Approve all and remount lost edits.
it("preserves edited action arguments across navigation away and back", () => {
  const first = mount();
  fireEvent.click(screen.getByRole("button", { name: settings.connected_apps.request.edit }));
  const edited = '{"to":"corrected@example.com"}';
  fireEvent.change(screen.getByRole("textbox"), { target: { value: edited } });
  first.unmount();
  mount();
  const edit = screen.queryByRole("button", { name: settings.connected_apps.request.edit });
  if (edit) fireEvent.click(edit);
  expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe(edited);
});

it("preserves an unsent rejection reason across navigation away and back", () => {
  const first = mount();
  fireEvent.click(screen.getByRole("button", { name: settings.connected_apps.request.reject }));
  fireEvent.change(screen.getByRole("textbox"), { target: { value: "This is the wrong recipient" } });
  first.unmount();
  mount();
  fireEvent.click(screen.getByRole("button", { name: settings.connected_apps.request.reject }));
  expect((screen.getByRole("textbox") as HTMLInputElement).value).toBe("This is the wrong recipient");
});

it("bulk approval sends the recipient visibly edited on the card", async () => {
  f.requests.push({ ...f.requests[0]!, id: "request-2" });
  mount();
  fireEvent.click(screen.getAllByRole("button", { name: settings.connected_apps.request.edit })[0]!);
  fireEvent.change(screen.getByRole("textbox"), { target: { value: '{"to":"corrected@example.com"}' } });
  fireEvent.click(screen.getByRole("button", { name: settings.connected_apps.request.approve_all }));
  await Promise.resolve();
  expect(f.mutateAsync).toHaveBeenCalledWith({ id: "request-1", decision: "approve", arguments: { to: "corrected@example.com" } });
});

it("control: individual approval sends the edited recipient", () => {
  mount();
  fireEvent.click(screen.getByRole("button", { name: settings.connected_apps.request.edit }));
  fireEvent.change(screen.getByRole("textbox"), { target: { value: '{"to":"corrected@example.com"}' } });
  fireEvent.click(screen.getByRole("button", { name: settings.connected_apps.request.approve }));
  expect(f.mutate).toHaveBeenCalledWith({ id: "request-1", decision: "approve", arguments: { to: "corrected@example.com" } }, expect.any(Object));
});

it("bulk approval with invalid edited JSON sends nothing", async () => {
  f.requests.push({ ...f.requests[0]!, id: "request-2" });
  mount();
  fireEvent.click(screen.getAllByRole("button", { name: settings.connected_apps.request.edit })[0]!);
  fireEvent.change(screen.getByRole("textbox"), { target: { value: "{not json" } });
  fireEvent.click(screen.getByRole("button", { name: settings.connected_apps.request.approve_all }));
  await Promise.resolve();
  expect(f.mutateAsync).not.toHaveBeenCalled();
});

it("clears the stored edit once the individual approval succeeds", () => {
  f.mutate.mockImplementation((_v: unknown, o: { onSuccess?: () => void }) => o.onSuccess?.());
  mount();
  fireEvent.click(screen.getByRole("button", { name: settings.connected_apps.request.edit }));
  fireEvent.change(screen.getByRole("textbox"), { target: { value: '{"to":"corrected@example.com"}' } });
  fireEvent.click(screen.getByRole("button", { name: settings.connected_apps.request.approve }));
  expect(useAppRequestDraftStore.getState().getDraft("request-1")).toBeUndefined();
  f.mutate.mockReset();
});
