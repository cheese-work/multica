// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type {
  AgentRuntime,
  MemberWithUser,
  RuntimePause,
} from "@multica/core/types";
import enRuntimes from "../../locales/en/runtimes.json";
import enCommon from "../../locales/en/common.json";
import type { RuntimeMachine } from "./runtime-machines";

const pauseMachine = vi.hoisted(() => vi.fn());
const resumeMachine = vi.hoisted(() => vi.fn());
const toastError = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { pauseRuntimeMachine: pauseMachine, resumeRuntimeMachine: resumeMachine },
}));
vi.mock("sonner", () => ({ toast: { error: toastError, success: vi.fn() } }));

import { MachinePauseCard } from "./machine-pause-card";

const MIN = 60_000;
const NOW = Date.parse("2026-10-30T09:00:00Z");

function runtime(overrides: Partial<AgentRuntime> = {}): AgentRuntime {
  return {
    id: "rt-1",
    workspace_id: "ws-1",
    daemon_id: "daemon-1",
    name: "Claude (x99)",
    runtime_mode: "local",
    provider: "claude",
    launch_header: "",
    status: "online",
    device_info: "x99",
    metadata: {},
    owner_id: "user-1",
    visibility: "private",
    last_seen_at: new Date(NOW).toISOString(),
    created_at: "",
    updated_at: "",
    pause: null,
    daemon_running_tasks: 3,
    ...overrides,
  };
}

function pause(overrides: Partial<RuntimePause> = {}): RuntimePause {
  return {
    reason: "maintenance",
    paused_by: "user-2",
    paused_at: new Date(NOW - 12 * MIN).toISOString(),
    expires_at: new Date(NOW + 108 * MIN).toISOString(),
    ...overrides,
  };
}

function machine(runtimes: AgentRuntime[]): RuntimeMachine {
  const online = runtimes.filter((item) => item.status === "online").length;
  return {
    id: "local:daemon-1",
    daemonId: "daemon-1",
    title: "x99",
    subtitle: "Linux amd64",
    deviceSummary: "Linux amd64",
    deviceInfo: "x99",
    cliVersion: null,
    launchedBy: null,
    update: { available: false, serverCommit: "", serverVersion: "", reason: "" },
    mode: "local",
    section: "remote",
    isCurrent: false,
    health: online > 0 ? "online" : "offline",
    runtimes,
    onlineCount: online,
    issueCount: 0,
    runningCount: 0,
    queuedCount: 0,
    providerNames: ["claude"],
    lastSeenAt: runtimes[0]?.last_seen_at ?? null,
  };
}

function member(userId: string, name: string, role: MemberWithUser["role"]): MemberWithUser {
  return {
    id: `m-${userId}`,
    workspace_id: "ws-1",
    user_id: userId,
    role,
    created_at: "",
    name,
    email: `${name}@example.com`,
    avatar_url: null,
  };
}

const MEMBERS = [
  member("user-1", "Cheese", "owner"),
  member("user-2", "Dana", "member"),
];

type CardProps = Partial<React.ComponentProps<typeof MachinePauseCard>>;

function ui(runtimes: AgentRuntime[], props: CardProps = {}) {
  return (
    <I18nProvider locale="en" resources={{ en: { runtimes: enRuntimes, common: enCommon } }}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { mutations: { retry: false } } })}
      >
        <MachinePauseCard
          machine={machine(runtimes)}
          wsId="ws-1"
          currentUserId="user-1"
          members={MEMBERS}
          now={NOW}
          {...props}
        />
      </QueryClientProvider>
    </I18nProvider>
  );
}

const card = () => screen.getByRole("region", { name: "Machine" });

describe("MachinePauseCard", () => {
  beforeEach(() => {
    pauseMachine.mockReset();
    resumeMachine.mockReset();
    toastError.mockReset();
  });

  it("running: shows the task count and pauses with the chosen reason", async () => {
    pauseMachine.mockResolvedValue(pause({ reason: "incident" }));
    render(ui([runtime()]));

    expect(within(card()).getByText("Taking work")).toBeInTheDocument();
    expect(within(card()).getByText("3 tasks running")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Pause/ }));
    expect(await screen.findByText("Stops new work. Running tasks finish. The pause ends by itself.")).toBeInTheDocument();
    expect(screen.getByText("Planned work. Ends after 2 h")).toBeInTheDocument();
    expect(screen.getByText("Something is wrong. Ends after 30 min")).toBeInTheDocument();
    expect(screen.getByText("Out of service. Ends after 30 min")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("menuitem", { name: /Incident/ }));
    await waitFor(() => expect(pauseMachine).toHaveBeenCalledWith("rt-1", "incident"));
  });

  it("pending: the Pause button is busy and disabled until the request settles", async () => {
    let finish: (value: RuntimePause) => void = () => {};
    pauseMachine.mockReturnValue(new Promise<RuntimePause>((resolve) => { finish = resolve; }));
    render(ui([runtime()]));

    fireEvent.click(screen.getByRole("button", { name: /Pause/ }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Maintenance/ }));

    const busy = await screen.findByRole("button", { name: /Pause/ });
    await waitFor(() => expect(busy).toHaveAttribute("aria-busy", "true"));
    expect(busy).toBeDisabled();
    finish(pause());
    await waitFor(() => expect(screen.getByRole("button", { name: /Pause/ })).not.toHaveAttribute("aria-busy", "true"));
  });

  it("pausing: draining count, who paused, and the time left; Resume lifts the pause", async () => {
    resumeMachine.mockResolvedValue(undefined);
    render(ui([runtime({ pause: pause() })]));

    expect(within(card()).getByText("Paused: maintenance")).toBeInTheDocument();
    expect(within(card()).getByText(/Draining: 3 tasks left/)).toBeInTheDocument();
    expect(within(card()).getByText(/by Dana,/)).toBeInTheDocument();
    expect(within(card()).getByText(/Ends in 1 h 48 min \(/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Resume" }));
    await waitFor(() => expect(resumeMachine).toHaveBeenCalledWith("rt-1"));
  });

  it("resume pending: the Resume button is busy and disabled", async () => {
    resumeMachine.mockReturnValue(new Promise(() => {}));
    render(ui([runtime({ pause: pause() })]));

    fireEvent.click(screen.getByRole("button", { name: "Resume" }));
    const busy = await screen.findByRole("button", { name: "Resume" });
    await waitFor(() => expect(busy).toHaveAttribute("aria-busy", "true"));
    expect(busy).toBeDisabled();
  });

  it("drained: says it is safe to shut down", () => {
    render(ui([runtime({ pause: pause(), daemon_running_tasks: 0 })]));
    expect(within(card()).getByText(/Safe to shut down/)).toBeInTheDocument();
    expect(within(card()).queryByText(/Draining/)).not.toBeInTheDocument();
  });

  it("offline and paused: keeps the clock and marks the count as the last report", () => {
    render(
      ui([
        runtime({
          status: "offline",
          pause: pause({ reason: "incident", expires_at: new Date(NOW + 12 * MIN).toISOString() }),
          daemon_running_tasks: 2,
        }),
      ]),
    );
    expect(within(card()).getByText("Paused: incident · Offline")).toBeInTheDocument();
    expect(within(card()).getByText(/Draining: 2 tasks left \(last report/)).toBeInTheDocument();
    expect(within(card()).getByText(/Ends in 12 min \(/)).toBeInTheDocument();
  });

  it("offline and not paused: Pause stays available", () => {
    render(ui([runtime({ status: "offline", daemon_running_tasks: 0 })]));
    expect(within(card()).getByText("Offline")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Pause/ })).toBeEnabled();
  });

  it("ending soon: the time left is emphasised", () => {
    render(
      ui([
        runtime({
          pause: pause({ expires_at: new Date(NOW + 4 * MIN).toISOString() }),
          daemon_running_tasks: 0,
        }),
      ]),
    );
    const ends = within(card()).getByText("Ends in 4 min");
    expect(ends.tagName).toBe("STRONG");
  });

  it("without rights: read-only, reason in text, Resume disabled, names to ask", () => {
    render(ui([runtime({ pause: pause() })], { currentUserId: "user-3" }));

    expect(within(card()).getByText("Paused: maintenance")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resume" })).toBeDisabled();
    expect(
      within(card()).getByRole("note"),
    ).toHaveTextContent(
      "Only the machine owner and the workspace owner can pause or resume. Ask Cheese.",
    );
  });

  it("without rights and not paused: Pause is disabled", () => {
    render(ui([runtime()], { currentUserId: "user-3" }));
    expect(screen.getByRole("button", { name: /Pause/ })).toBeDisabled();
    expect(within(card()).getByRole("note")).toBeInTheDocument();
  });

  it("load failed: says so, offers Retry, and turns Pause off", () => {
    const retry = vi.fn();
    render(ui([runtime()], { loadFailed: true, onRetry: retry }));

    expect(within(card()).getByText("Machine state not loaded")).toBeInTheDocument();
    expect(within(card()).getByRole("alert")).toHaveTextContent(
      "Could not load the machine state. Check your connection and retry.",
    );
    expect(screen.getByRole("button", { name: /Pause/ })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it("ended: names the pause that ran out once the clock passes its expiry", () => {
    const live = runtime({ pause: pause({ expires_at: new Date(NOW + 10 * MIN).toISOString() }) });
    const { rerender } = render(ui([live]));
    expect(within(card()).getByText("Paused: maintenance")).toBeInTheDocument();

    // The server hides the pause at the expiry; the client clock moves past it.
    rerender(ui([runtime({ pause: null, daemon_running_tasks: 0 })], { now: NOW + 11 * MIN }));
    expect(within(card()).getByText("Taking work")).toBeInTheDocument();
    expect(within(card()).getByRole("status")).toHaveTextContent(
      /The maintenance pause ended at .+ x99 is taking work again\./,
    );
  });

  it("resumed early: shows no ended notice", async () => {
    resumeMachine.mockResolvedValue(undefined);
    const live = runtime({ pause: pause({ expires_at: new Date(NOW + 10 * MIN).toISOString() }) });
    const { rerender } = render(ui([live]));
    rerender(ui([runtime({ pause: null, daemon_running_tasks: 0 })], { now: NOW + 1 * MIN }));
    rerender(ui([runtime({ pause: null, daemon_running_tasks: 0 })], { now: NOW + 20 * MIN }));
    expect(within(card()).queryByText(/pause ended at/)).not.toBeInTheDocument();
  });

  it("a failed request reports through a toast", async () => {
    pauseMachine.mockRejectedValue(new Error("forbidden"));
    render(ui([runtime()]));
    fireEvent.click(screen.getByRole("button", { name: /Pause/ }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Suspended/ }));
    await waitFor(() => expect(toastError).toHaveBeenCalledWith("Could not pause the machine"));
  });
});
