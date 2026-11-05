import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

// A mocked module's factory runs when the module is first imported.
const loads = vi.hoisted(() => ({ autopilot: 0 }));

vi.mock("../autopilots/components/autopilot-dialog", () => {
  loads.autopilot += 1;
  return { AutopilotDialog: () => <div>autopilot-dialog</div> };
});
vi.mock("../issues/components/terminate-task-confirm-dialog", () => ({
  TerminateTaskConfirmDialog: () => null,
}));
vi.mock("../i18n", () => ({ useT: () => ({ t: () => "" }) }));

import { PaletteHostDialogs } from "./host-dialogs";

afterEach(cleanup);

describe("PaletteHostDialogs", () => {
  it("does not load the autopilot form (and its editor) until it is opened", () => {
    render(<PaletteHostDialogs dialog={null} onClose={() => {}} />);

    expect(loads.autopilot).toBe(0);
  });

  it("shows the autopilot form when the palette asks for one", async () => {
    render(<PaletteHostDialogs dialog={{ type: "new-autopilot" }} onClose={() => {}} />);

    expect(await screen.findByText("autopilot-dialog")).toBeTruthy();
  });
});
