import { useEffect, useImperativeHandle, useRef, useState } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";

// CHE-3523: unsent create-autopilot input lives in the keyed draft store, so it
// survives the dialog unmounting and clears only after a successful create.
const mockCreateAutopilot = vi.hoisted(() => vi.fn());
const mockCreateTrigger = vi.hoisted(() => vi.fn());
const mockUpdateAutopilot = vi.hoisted(() => vi.fn());

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
  useUpdateAutopilot: () => ({ mutateAsync: mockUpdateAutopilot }),
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
  // Mirrors the real editor's lifecycle: `onUpdate` is debounced, and a pending
  // update is dropped on unmount unless `flushPendingOnUnmount` is set.
  ContentEditor: ({
    ref,
    defaultValue,
    placeholder,
    onUpdate,
    debounceMs = 0,
    flushPendingOnUnmount = false,
  }: any) => {
    const [value, setValue] = useState(defaultValue ?? "");
    const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
    const pending = useRef<string | null>(null);
    const onUpdateRef = useRef(onUpdate);
    onUpdateRef.current = onUpdate;
    // Same contract as the real handle: the latest text if an update is still
    // pending, and null when the host already has everything.
    useImperativeHandle(ref, () => ({
      flushPendingUpdate: () => {
        if (pending.current === null) return null;
        clearTimeout(timer.current);
        const md = pending.current;
        pending.current = null;
        return md;
      },
    }));
    useEffect(
      () => () => {
        clearTimeout(timer.current);
        if (flushPendingOnUnmount && pending.current !== null) {
          onUpdateRef.current?.(pending.current);
        }
      },
      [flushPendingOnUnmount],
    );
    return (
      <textarea
        aria-label="runbook"
        placeholder={placeholder}
        value={value}
        onChange={(e) => {
          setValue(e.target.value);
          pending.current = e.target.value;
          clearTimeout(timer.current);
          timer.current = setTimeout(() => {
            pending.current = null;
            onUpdateRef.current?.(e.target.value);
          }, debounceMs);
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

function renderEditDialog(onOpenChange = vi.fn()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <AutopilotDialog
        mode="edit"
        open
        onOpenChange={onOpenChange}
        autopilotId="ap-1"
        initial={{
          title: "Issue title sweep",
          description: "",
          project_id: null,
          assignee_type: "agent",
          assignee_id: "agent-1",
          execution_mode: "run_only",
          subscriber_user_ids: [],
        }}
        triggers={[]}
        collaborators={[]}
        canManageAccess={false}
      />
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
    mockUpdateAutopilot.mockReset();
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
  it("keeps runbook text typed just before the dialog closes", async () => {
    const user = userEvent.setup();
    const first = renderCreateDialog();
    await user.type(screen.getByLabelText("runbook"), "Summarise the inbox");
    // Closed inside the editor's 300ms debounce window.
    first.unmount();

    renderCreateDialog();
    expect(screen.getByLabelText("runbook")).toHaveValue("Summarise the inbox");
  });

  it("does not bring runbook text back after a successful create", async () => {
    const user = userEvent.setup();
    mockCreateAutopilot.mockResolvedValue({ id: "ap-1" });
    mockCreateTrigger.mockResolvedValue({ id: "tr-1" });
    const first = renderCreateDialog();
    await user.type(screen.getByLabelText("title"), "Daily digest");
    await user.click(assigneeTrigger());
    await user.click(await screen.findByRole("button", { name: /Scout/ }));
    await user.type(screen.getByLabelText("runbook"), "Summarise the inbox");
    // Submit while that runbook update is still pending in the editor.
    await user.click(createButton());
    await waitFor(() => expect(mockCreateTrigger).toHaveBeenCalled());
    // The request carries the text typed right before Save, not the lagging state.
    expect(mockCreateAutopilot).toHaveBeenCalledWith(
      expect.objectContaining({ description: "Summarise the inbox" }),
    );
    first.unmount();

    renderCreateDialog();
    expect(screen.getByLabelText("title")).toHaveValue("");
    expect(screen.getByLabelText("runbook")).toHaveValue("");
  });

  it("keeps the runbook typed just before a Save that fails", async () => {
    const user = userEvent.setup();
    mockCreateAutopilot.mockRejectedValueOnce(new Error("nope"));
    const first = renderCreateDialog();
    await user.type(screen.getByLabelText("title"), "Daily digest");
    await user.click(assigneeTrigger());
    await user.click(await screen.findByRole("button", { name: /Scout/ }));
    await user.type(screen.getByLabelText("runbook"), "Summarise the inbox");
    await user.click(createButton());
    await waitFor(() => expect(mockCreateAutopilot).toHaveBeenCalledTimes(1));
    first.unmount();

    renderCreateDialog();
    expect(screen.getByLabelText("runbook")).toHaveValue("Summarise the inbox");
  });

  it("saves the runbook typed just before Save on an edit", async () => {
    const user = userEvent.setup();
    mockUpdateAutopilot.mockResolvedValue({ id: "ap-1" });
    renderEditDialog();
    await user.type(screen.getByLabelText("runbook"), "Fresh steps");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(mockUpdateAutopilot).toHaveBeenCalled());
    expect(mockUpdateAutopilot).toHaveBeenCalledWith(
      expect.objectContaining({ id: "ap-1", description: "Fresh steps" }),
    );
  });

  it("retries a failed trigger on the autopilot it already created", async () => {
    const user = userEvent.setup();
    mockCreateAutopilot.mockResolvedValue({ id: "ap-1" });
    mockUpdateAutopilot.mockResolvedValue({ id: "ap-1" });
    mockCreateTrigger.mockRejectedValueOnce(new Error("bad cron")).mockResolvedValue({ id: "tr-1" });
    const first = renderCreateDialog();
    await user.type(screen.getByLabelText("title"), "Daily digest");
    await user.click(assigneeTrigger());
    await user.click(await screen.findByRole("button", { name: /Scout/ }));
    await user.click(createButton());
    await waitFor(() => expect(mockCreateTrigger).toHaveBeenCalledTimes(1));
    const failedTrigger = mockCreateTrigger.mock.calls[0]![0];
    // Closed or reloaded before the retry: the draft must remember the autopilot.
    first.unmount();

    const second = renderCreateDialog();
    expect(screen.getByLabelText("title")).toHaveValue("Daily digest");
    await user.click(createButton());
    await waitFor(() => expect(mockCreateTrigger).toHaveBeenCalledTimes(2));
    expect(mockCreateAutopilot).toHaveBeenCalledTimes(1);
    expect(mockUpdateAutopilot).toHaveBeenCalledWith(
      expect.objectContaining({ id: "ap-1", title: "Daily digest" }),
    );
    expect(mockCreateTrigger.mock.calls[1]![0]).toEqual(failedTrigger);
    second.unmount();

    renderCreateDialog();
    expect(screen.getByLabelText("title")).toHaveValue("");
  });
  it("keeps an edit open and retries the schedule when only the trigger write fails", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();
    mockUpdateAutopilot.mockResolvedValue({ id: "ap-1" });
    mockCreateTrigger.mockRejectedValueOnce(new Error("bad cron")).mockResolvedValue({ id: "tr-1" });
    const first = renderEditDialog(onOpenChange);
    await user.click(screen.getByRole("button", { name: "Add schedule" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(mockCreateTrigger).toHaveBeenCalledTimes(1));
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
    first.unmount();

    // Reopened: the unsaved schedule is still there to retry.
    renderEditDialog(onOpenChange);
    expect(
      await screen.findByText("Once saved, runs automatically until paused."),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(mockCreateTrigger).toHaveBeenCalledTimes(2));
    expect(mockCreateTrigger.mock.calls[1]![0]).toEqual(mockCreateTrigger.mock.calls[0]![0]);
  });
});
