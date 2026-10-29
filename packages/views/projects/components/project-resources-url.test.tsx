import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";

const { remove, resource } = vi.hoisted(() => ({
  remove: vi.fn().mockResolvedValue(undefined),
  resource: {
    id: "url-resource",
    project_id: "p1",
    workspace_id: "workspace-1",
    resource_type: "url",
    resource_ref: { url: "https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod" },
    label: "doop design file",
    position: 0,
    created_at: "2026-10-29T00:00:00Z",
    created_by: null,
  },
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: { queryKey?: unknown[] }) => ({
    data: options.queryKey?.[0] === "project-resources" ? [resource] : [],
  }),
  queryOptions: (options: unknown) => options,
}));
vi.mock("@multica/core/projects", () => ({
  projectResourcesOptions: () => ({ queryKey: ["project-resources"] }),
  useCreateProjectResource: () => ({ mutateAsync: vi.fn() }),
  useUpdateProjectResource: () => ({ mutateAsync: vi.fn() }),
  useDeleteProjectResource: () => ({ mutateAsync: remove }),
}));
vi.mock("@multica/core/config", () => ({
  useConfigStore: (selector: (state: { localWorktreeSupported: boolean }) => unknown) =>
    selector({ localWorktreeSupported: false }),
}));
vi.mock("@multica/core/runtimes", () => ({
  runtimeListOptions: () => ({ queryKey: ["runtimes"] }),
  runtimeAdvertisesLocalWorktree: () => false,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "workspace-1", slug: "ws", repos: [] }),
}));
vi.mock("../../platform/local-directory", () => ({
  isDesktopShell: () => false,
  pickDirectory: vi.fn(),
  validateLocalDirectory: vi.fn(),
}));
vi.mock("../../platform/use-local-daemon-status", () => ({
  useLocalDaemonStatus: () => ({ daemonId: null, running: false }),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { ProjectResourcesSection } from "./project-resources-section";

describe("ProjectResourcesSection URL resource", () => {
  it("shows the HTTPS link and its note separately", () => {
    renderWithI18n(<ProjectResourcesSection projectId="p1" />);
    const link = screen.getByRole("link", { name: resource.resource_ref.url });
    expect(link).toHaveAttribute("href", resource.resource_ref.url);
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
    expect(screen.getByText("doop design file")).toBeInTheDocument();
    expect(link).not.toHaveTextContent("doop design file");
    expect(screen.queryByText("Default branch")).not.toBeInTheDocument();
  });

  it("keeps the remove action wired to the URL resource", () => {
    renderWithI18n(<ProjectResourcesSection projectId="p1" />);
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    expect(remove).toHaveBeenCalledWith(resource.id);
  });
});
