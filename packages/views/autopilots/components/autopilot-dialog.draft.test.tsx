import { useImperativeHandle, useRef, useState } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";

// CHE-3523: unsent create-autopilot input lives in the keyed draft store, so it
// survives the dialog unmounting and clears only after a successful create.
const mockCreateAutopilot = vi.hoisted(() => vi.fn());
const mockCreateTrigger = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-test" }));
vi.mock("@multica/core/paths", () => ({ useCurrentWorkspace: () => ({ name: "Acme" }) }));

vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: (wsId: string) => ({
    queryKey: ["agents", wsId],
    queryFn: async () => [
      {
        id: "agent-1",
        name: "Scout",
        description: "Researches things",
        archived_at: null,
        runtime_id: "runtime-1",
      },
    ],
  }),
  squadListOptions: (wsId: string) => ({
    queryKey: ["squads", wsId],
    queryFn: async () => [],
  }),
}));

vi.mock("@multica/core/projects/queries", () => ({
  projectListOptions: (wsId: string) => ({
    queryKey: ["projects", wsId],
    queryFn: async () => [],
  }),
}));

vi.mock("@multica/core/autopilots/queries", () => ({
  cronPreviewOptions: (wsId: string, expr: string, tz: string) => ({
    queryKey: ["cron-preview", wsId, expr, tz],
    queryFn: async () => ({ next_runs: ["2126-07-14T01:00:00Z"] }),
    retry: false,
  }),
}));

vi.mock("@multica/core/autopilots/mutations", () => ({
  useCreateAutopilot: () => ({ mutateAsync: mockCreateAutopilot }),
  useCreateAutopilotTrigger: () => ({ mutateAsync: mockCreateTrigger }),
  useUpdateAutopilot: () => ({ mutateAsync: vi.fn() }),
  useUpdateAutopilotTrigger: () => ({ mutateAsync: vi.fn() }),
}));

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

// Tiptap in jsdom is neither cheap nor the subject here: the title is a plain
// input whose ref honours focus(), which is all the dialog asks of it.
vi.mock("../../editor", () => ({
  TitleEditor: ({ ref, defaultValue, placeholder, onChange, onSubmit }: any) => {
    const [value, setValue] = useState(defaultValue ?? "");
    const inputRef = useRef<HTMLInputElement>(null);
    useImperativeHandle(ref, () => ({
      getText: () => value,
      focus: () => inputRef.current?.focus(),
      focusAtCoords: () => inputRef.current?.focus(),
    }));
    return (
      <input
        ref={inputRef}
        aria-label="title"
        value={value}
        placeholder={placeholder}
        onChange={(e) => {
          setValue(e.target.value);
          onChange?.(e.target.value);
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter") onSubmit?.();
        }}
      />
    );
  },
  ContentEditor: ({ defaultValue, placeholder, onUpdate }: any) => {
    const [value, setValue] = useState(defaultValue ?? "");
    return (
      <textarea
        aria-label="runbook"
        placeholder={placeholder}
        value={value}
        onChange={(e) => {
          setValue(e.target.value);
          onUpdate?.(e.target.value);
        }}
      />
    );
  },
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorId }: { actorId: string }) => <span data-testid="actor-avatar">{actorId}</span>,
}));

vi.mock("./subscriber-multi-select", () => ({
  SubscriberMultiSelect: () => <div data-testid="subscriber-multi-select" />,
}));

vi.mock("../../projects/components/project-picker", () => ({
  ProjectPicker: ({ triggerRender }: { triggerRender: React.ReactElement }) => triggerRender,
}));

vi.mock("../pickers/timezone-picker", () => ({
  TimezonePicker: ({ value }: { value: string }) => <div data-testid="timezone-picker">{value}</div>,
}));

import { useAutopilotDialogDraftStore } from "@multica/core/autopilots/stores";
import { AutopilotDialog } from "./autopilot-dialog";

function renderCreateDialog() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <AutopilotDialog mode="create" open onOpenChange={vi.fn()} />
    </QueryClientProvider>,
  );
}

const createButton = () => screen.getByRole("button", { name: "Create autopilot" });
const assigneeTrigger = () => screen.getByRole("button", { name: /Select agent or squad/ });

describe("AutopilotDialog draft persistence", () => {
  beforeEach(() => {
    useAutopilotDialogDraftStore.setState({ drafts: {} });
    mockCreateAutopilot.mockReset();
    mockCreateTrigger.mockReset();
  });

  it("restores title, runbook and assignee after an unmount and remount", async () => {
    const user = userEvent.setup();
    const first = renderCreateDialog();
    await user.type(screen.getByLabelText("title"), "Daily digest");
    await user.type(screen.getByLabelText("runbook"), "Summarise the inbox");
    await user.click(assigneeTrigger());
    await user.click(await screen.findByRole("button", { name: /Scout/ }));
    first.unmount();

    renderCreateDialog();
    expect(screen.getByLabelText("title")).toHaveValue("Daily digest");
    expect(screen.getByLabelText("runbook")).toHaveValue("Summarise the inbox");
    expect(await screen.findByRole("button", { name: /Scout/ })).toBeInTheDocument();
  });

  it("keeps the draft when the create fails and clears it after a successful create", async () => {
    const user = userEvent.setup();
    mockCreateAutopilot.mockRejectedValueOnce(new Error("nope"));
    const first = renderCreateDialog();
    await user.type(screen.getByLabelText("title"), "Daily digest");
    await user.click(assigneeTrigger());
    await user.click(await screen.findByRole("button", { name: /Scout/ }));
    await user.click(createButton());
    await waitFor(() => expect(mockCreateAutopilot).toHaveBeenCalledTimes(1));
    first.unmount();

    mockCreateAutopilot.mockResolvedValueOnce({ id: "ap-1" });
    mockCreateTrigger.mockResolvedValue({ id: "tr-1" });
    const second = renderCreateDialog();
    expect(screen.getByLabelText("title")).toHaveValue("Daily digest");
    await user.click(createButton());
    await waitFor(() => expect(mockCreateAutopilot).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(mockCreateTrigger).toHaveBeenCalled());
    second.unmount();

    renderCreateDialog();
    expect(screen.getByLabelText("title")).toHaveValue("");
  });
});
