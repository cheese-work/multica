import { act, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { paths, WorkspaceSlugProvider } from "@multica/core/paths";
import { useSearchStore } from "@multica/views/search";

const state = vi.hoisted(() => ({
  pathname: "/acme/search",
  params: new URLSearchParams(),
  router: { replace: vi.fn() },
  workspace: { slug: "acme", name: "Acme", issue_prefix: "C9" } as { slug: string; name: string; issue_prefix: string } | null,
}));

vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...await importOriginal<typeof import("@multica/core/paths")>(),
  useCurrentWorkspace: () => state.workspace,
}));

vi.mock("next/navigation", () => ({
  usePathname: () => state.pathname,
  useSearchParams: () => state.params,
  useRouter: () => state.router,
}));

vi.mock("@multica/views/search", async (importOriginal) => {
  const original = await importOriginal<typeof import("@multica/views/search")>();
  return {
    ...original,
    SearchCommand: ({ initialQuery }: { initialQuery?: string }) => (
      <output data-testid="query">{initialQuery ?? "keyboard"}</output>
    ),
  };
});

import { WorkspaceSearchCommand } from "./workspace-search-command";
import SearchPage from "../app/[workspaceSlug]/(dashboard)/search/page";

function renderCommand() {
  return render(<WorkspaceSearchCommand />, {
    wrapper: ({ children }) => <WorkspaceSlugProvider slug="acme">{children}</WorkspaceSlugProvider>,
  });
}

beforeEach(() => {
  state.workspace = { slug: "acme", name: "Acme", issue_prefix: "C9" };
  state.pathname = "/acme/search";
  state.params = new URLSearchParams();
  state.router.replace.mockReset();
  useSearchStore.setState({ open: false });
});

describe("workspace search URL", () => {
  it("waits for workspace membership before interpreting an identifier", () => {
    state.workspace = null;
    state.params = new URLSearchParams({ q: "C9-123 title" });
    const view = renderCommand();
    expect(state.router.replace).not.toHaveBeenCalled();
    expect(useSearchStore.getState().open).toBe(false);
    state.workspace = { slug: "acme", name: "Acme", issue_prefix: "C9" };
    view.rerender(<WorkspaceSearchCommand />);
    expect(state.router.replace).toHaveBeenCalledExactlyOnceWith("/acme/issues/C9-123");
  });

  it("uses the existing legacy name prefix for old workspaces", () => {
    state.workspace = { slug: "acme-team", name: "Acme Team", issue_prefix: "" };
    state.params = new URLSearchParams({ q: "ACM-123 title" });
    renderCommand();
    expect(state.router.replace).toHaveBeenCalledExactlyOnceWith("/acme/issues/ACM-123");
  });
  it.each(["C9-123 title from suggestions", "c9-123"])("opens the suggested issue for %s", (query) => {
    state.params = new URLSearchParams({ q: query });
    renderCommand();
    expect(state.router.replace).toHaveBeenCalledExactlyOnceWith("/acme/issues/C9-123");
    expect(useSearchStore.getState().open).toBe(false);
  });

  it("keeps another workspace's identifier in the palette", () => {
    state.params = new URLSearchParams({ q: "OTHER-123 title" });
    renderCommand();
    expect(useSearchStore.getState().open).toBe(true);
    expect(state.router.replace).not.toHaveBeenCalled();
  });
  it("has a dashboard route", () => {
    expect(SearchPage()).toBeNull();
  });

  it("opens the palette with the decoded q value", () => {
    state.params = new URLSearchParams("q=login+%2B+%E4%B8%AD%E6%96%87");
    renderCommand();
    expect(useSearchStore.getState().open).toBe(true);
    expect(screen.getByTestId("query")).toHaveTextContent("login + 中文");
    expect(state.router.replace).not.toHaveBeenCalled();
  });

  it.each(["", "q="])("opens an empty palette for %s", (query) => {
    state.params = new URLSearchParams(query);
    renderCommand();
    expect(useSearchStore.getState().open).toBe(true);
    expect(screen.getByTestId("query").textContent).toBe("");
  });

  it("replaces rather than pushes the URL when closed, without reopening", () => {
    const view = renderCommand();
    act(() => useSearchStore.getState().setOpen(false));
    expect(state.router.replace).toHaveBeenCalledExactlyOnceWith(paths.workspace("acme").root());
    view.rerender(<WorkspaceSearchCommand />);
    expect(useSearchStore.getState().open).toBe(false);
    state.pathname = "/acme/issues";
    view.rerender(<WorkspaceSearchCommand />);
    expect(useSearchStore.getState().open).toBe(false);
    expect(state.router.replace).toHaveBeenCalledTimes(1);
  });

  it("does not revisit the palette when the browser goes Back after closing", async () => {
    window.history.replaceState(null, "", "/acme/projects");
    window.history.pushState(null, "", "/acme/search?q=login");
    state.router.replace.mockImplementation((destination: string) => {
      window.history.replaceState(null, "", destination);
    });
    const view = renderCommand();
    act(() => useSearchStore.getState().setOpen(false));
    expect(window.location.pathname).toBe("/acme/issues");
    const wentBack = new Promise<void>((resolve) => {
      window.addEventListener("popstate", () => resolve(), { once: true });
    });
    window.history.back();
    await wentBack;
    state.pathname = window.location.pathname;
    view.rerender(<WorkspaceSearchCommand />);
    expect(state.pathname).toBe("/acme/projects");
    expect(useSearchStore.getState().open).toBe(false);
  });

  it("leaves normal keyboard search unchanged and closes on leaving the URL", () => {
    state.pathname = "/acme/issues";
    const view = renderCommand();
    expect(useSearchStore.getState().open).toBe(false);
    expect(screen.getByTestId("query")).toHaveTextContent("keyboard");
    state.pathname = "/acme/search";
    view.rerender(<WorkspaceSearchCommand />);
    expect(useSearchStore.getState().open).toBe(true);
    state.pathname = "/acme/projects";
    view.rerender(<WorkspaceSearchCommand />);
    expect(useSearchStore.getState().open).toBe(false);
    expect(state.router.replace).not.toHaveBeenCalled();
  });
});
