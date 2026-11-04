// @vitest-environment jsdom

import { describe, expect, it, vi, afterEach } from "vitest";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Agent } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { AgentDetailInspector } from "./agent-detail-inspector";

vi.mock("@tanstack/react-query", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-query")>()),
  useQuery: () => ({ data: undefined, isSuccess: false }),
}));
vi.mock("../../common/avatar-upload-control", () => ({
  AvatarUploadControl: () => <div />,
}));
vi.mock("./inspector/model-picker", () => ({ ModelPicker: () => <div /> }));
vi.mock("./inspector/runtime-picker", () => ({ RuntimePicker: () => <div /> }));
vi.mock("./inspector/thinking-prop-row", () => ({ ThinkingSettingField: () => <div /> }));
vi.mock("./inspector/service-tier-setting-field", () => ({
  ServiceTierSettingField: () => <div />,
}));

const agent = {
  id: "agent-draft",
  workspace_id: "workspace-1",
  name: "Lambda",
  description: "Coding agent",
  runtime_id: "runtime-1",
  avatar_url: null,
  conversation_starters: [],
  max_concurrent_tasks: 1,
} as unknown as Agent;

function renderInspector(onUpdate = vi.fn().mockRejectedValue(new Error("offline"))) {
  return renderWithI18n(
    <AgentDetailInspector
      agent={agent}
      runtime={null}
      runtimes={[]}
      members={[]}
      currentUserId="user-1"
      canEdit
      onUpdate={onUpdate}
    />,
  );
}

afterEach(() => resetAllRegisteredDrafts());

describe("AgentDetailInspector drafts", () => {
  it("keeps an unsaved name and description across unmount and remount", async () => {
    const user = userEvent.setup();
    const first = renderInspector();
    const name = screen.getByLabelText("Name");
    await user.clear(name);
    await user.type(name, "Renamed");
    const description = screen.getByLabelText("Description");
    await user.clear(description);
    await user.type(description, "New blurb");
    first.unmount();

    renderInspector();
    expect(screen.getByLabelText("Name")).toHaveValue("Renamed");
    expect(screen.getByLabelText("Description")).toHaveValue("New blurb");
  });
});
