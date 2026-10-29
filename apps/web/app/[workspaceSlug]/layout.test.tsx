import { Suspense, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

const state = vi.hoisted(() => ({
  user: {
    id: "user-1",
    onboarded_at: "2026-01-01T00:00:00Z",
  } as { id: string; onboarded_at: string | null } | null,
  isAuthLoading: false,
  workspace: null as { id: string; slug: string } | null,
  workspacesBySlug: {} as Record<string, { id: string; slug: string } | null>,
  workspaceError: false,
  hasBeenSeen: false,
  pathname: "/acme/issues",
  replace: vi.fn(),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: state.replace }),
  usePathname: () => state.pathname,
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: (
    selector: (auth: {
      user: typeof state.user;
      isLoading: boolean;
    }) => unknown,
  ) => selector({ user: state.user, isLoading: state.isAuthLoading }),
}));

vi.mock("@multica/core/workspace", () => ({
  workspaceBySlugOptions: (slug: string) => ({
    queryKey: ["workspace-by-slug", slug],
    queryFn: async () => {
      if (state.workspaceError) throw new Error("temporary failure");
      if (slug in state.workspacesBySlug) return state.workspacesBySlug[slug];
      return state.workspace;
    },
  }),
}));

vi.mock("@multica/core/paths", () => ({
  WorkspaceSlugProvider: ({ children }: { children: ReactNode }) => (
    <>{children}</>
  ),
  paths: {
    login: () => "/login",
    onboarding: () => "/onboarding",
  },
}));

vi.mock("@multica/core/platform", () => ({
  setCurrentWorkspace: vi.fn(),
}));

vi.mock("@multica/views/workspace/no-access-page", () => ({
  NoAccessPage: () => <div data-testid="no-access" />,
}));

vi.mock("@multica/views/workspace/use-workspace-seen", () => ({
  useWorkspaceSeen: () => state.hasBeenSeen,
}));

vi.mock("@multica/ui/components/common/multica-icon", () => ({
  MulticaIcon: () => <div data-testid="workspace-loading" />,
}));

import { setCurrentWorkspace } from "@multica/core/platform";
import WorkspaceLayout from "./layout";

/** `use()` unwraps a params promise synchronously only when it is already
 *  settled, which is how the App Router hands it over at runtime. */
function makeParams(workspaceSlug: string) {
  return Object.assign(Promise.resolve({ workspaceSlug }), {
    status: "fulfilled",
    value: { workspaceSlug },
  });
}

function renderTree(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: Infinity } },
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
  const result = render(
    <Suspense fallback={<div data-testid="route-loading" />}>{node}</Suspense>,
    { wrapper },
  );
  return { ...result, queryClient };
}

function renderLayout(slug = "acme") {
  return renderTree(
    <WorkspaceLayout params={makeParams(slug)}>
      <div data-testid="workspace-content" />
    </WorkspaceLayout>,
  );
}

function renderStaticLayout(slug = "acme") {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: Infinity } },
  });
  return renderToStaticMarkup(
    <QueryClientProvider client={queryClient}>
      <WorkspaceLayout params={makeParams(slug)}>
        <div data-testid="workspace-content" />
      </WorkspaceLayout>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  state.user = {
    id: "user-1",
    onboarded_at: "2026-01-01T00:00:00Z",
  };
  state.isAuthLoading = false;
  state.workspace = null;
  state.workspacesBySlug = {};
  state.workspaceError = false;
  state.hasBeenSeen = false;
  state.pathname = "/acme/issues";
  window.history.replaceState(null, "", "/acme/issues");
});

describe("WorkspaceLayout", () => {
  it("renders the matching OpenSearch link in initial HTML before auth hydration", () => {
    state.isAuthLoading = true;

    const html = renderStaticLayout();

    expect(html).toContain('rel="search"');
    expect(html).toContain('href="/acme/opensearch.xml"');
    expect(html).not.toContain('data-testid="workspace-content"');
  });

  it("renders only the routed workspace discovery link during an initial transition", () => {
    state.isAuthLoading = true;
    state.pathname = "/globex/issues";
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: Infinity } },
    });

    const html = renderToStaticMarkup(
      <QueryClientProvider client={queryClient}>
        <>
          <WorkspaceLayout params={makeParams("acme")}>
            <div data-testid="stale-workspace-content" />
          </WorkspaceLayout>
          <WorkspaceLayout params={makeParams("globex")}>
            <div data-testid="routed-workspace-content" />
          </WorkspaceLayout>
        </>
      </QueryClientProvider>,
    );

    expect(html.match(/rel="search"/g)).toHaveLength(1);
    expect(html).toContain('href="/globex/opensearch.xml"');
    expect(html).not.toContain('href="/acme/opensearch.xml"');
  });

  it("advertises the workspace OpenSearch document in the document head", async () => {
    state.workspace = { id: "workspace-acme", slug: "acme" };
    renderLayout();
    await screen.findByTestId("workspace-content");
    const link = document.head.querySelector('link[rel="search"]');
    expect(link).toHaveAttribute("type", "application/opensearchdescription+xml");
    expect(link).toHaveAttribute("href", "/acme/opensearch.xml");
    expect(link).toHaveAttribute("title", "Multica acme");
  });

  it("does not advertise a workspace outside the membership list", async () => {
    renderLayout();
    await screen.findByTestId("no-access");
    expect(document.head.querySelector('link[rel="search"]')).toBeNull();
  });

  it("does not attach the old workspace target when a verified session ends", async () => {
    state.pathname = "/acme/search";
    state.workspace = { id: "workspace-acme", slug: "acme" };
    window.history.replaceState(null, "", "/acme/search?q=old-user-query");
    const view = renderLayout();
    expect(await screen.findByTestId("workspace-content")).toBeInTheDocument();
    state.user = null;
    view.rerender(
      <Suspense fallback={<div data-testid="route-loading" />}>
        <WorkspaceLayout params={makeParams("acme")}>
          <div data-testid="workspace-content" />
        </WorkspaceLayout>
      </Suspense>,
    );
    await waitFor(() => expect(state.replace).toHaveBeenCalledWith("/login"));
    expect(state.replace).not.toHaveBeenCalledWith(expect.stringContaining("next="));
  });

  it("preserves the search target when an unverified cached session is rejected", async () => {
    state.pathname = "/acme/search";
    state.isAuthLoading = true;
    const destination = "/acme/search?q=fresh-visit";
    window.history.replaceState(null, "", destination);
    const view = renderLayout();
    state.user = null;
    state.isAuthLoading = false;
    view.rerender(
      <Suspense fallback={<div data-testid="route-loading" />}>
        <WorkspaceLayout params={makeParams("acme")}>
          <div data-testid="workspace-content" />
        </WorkspaceLayout>
      </Suspense>,
    );
    await waitFor(() => expect(state.replace).toHaveBeenCalledWith(`/login?next=${encodeURIComponent(destination)}`));
  });

  it("preserves the search URL and encoded query when login is required", async () => {
    state.user = null;
    state.pathname = "/acme/search";
    const destination = "/acme/search?q=login%20%2B%20%E4%B8%AD%E6%96%87";
    window.history.replaceState(null, "", destination);
    renderLayout();
    await waitFor(() => expect(state.replace).toHaveBeenCalledWith(`/login?next=${encodeURIComponent(destination)}`));
    expect(screen.queryByTestId("workspace-content")).not.toBeInTheDocument();
  });

  it("does not mount the search route for a workspace outside the membership list", async () => {
    state.pathname = "/acme/search";
    renderLayout();
    expect(await screen.findByTestId("no-access")).toBeInTheDocument();
    expect(screen.queryByTestId("workspace-content")).not.toBeInTheDocument();
  });

  it("keeps loading instead of showing NoAccess when the initial list request fails", async () => {
    state.workspaceError = true;
    const { queryClient } = renderLayout();

    await waitFor(() => {
      expect(
        queryClient.getQueryState(["workspace-by-slug", "acme"])?.status,
      ).toBe("error");
    });
    expect(screen.queryByTestId("no-access")).toBeNull();
    expect(screen.getByTestId("workspace-loading")).toBeInTheDocument();
  });

  it("shows NoAccess only after an authoritative list omits the slug", async () => {
    renderLayout();

    expect(await screen.findByTestId("no-access")).toBeInTheDocument();
    expect(screen.queryByTestId("workspace-loading")).toBeNull();
  });
});

/**
 * The platform workspace singleton is a module global written from render, so
 * it is only safe while exactly one layout claims it. The App Router can keep
 * a previous workspace's layout mounted beside the incoming one; unguarded,
 * both wrote on every render and the singleton alternated indefinitely. That
 * churn tore down and rebuilt the realtime socket, which binds to this slug,
 * and re-pointed the @mention lookup, which reads it synchronously and returns
 * an empty list for a workspace with no warm cache. The live pathname is the
 * tiebreak: one value shared by every instance, so only the routed one writes.
 */
describe("WorkspaceLayout workspace singleton", () => {
  it("adopts the workspace when its slug is the routed one", async () => {
    state.pathname = "/acme/issues";
    state.workspacesBySlug = { acme: { id: "ws-acme", slug: "acme" } };

    renderLayout("acme");

    await waitFor(() => {
      expect(setCurrentWorkspace).toHaveBeenCalledWith("acme", "ws-acme");
    });
  });

  it("stays silent while another workspace owns the URL", async () => {
    state.pathname = "/globex/issues";
    state.workspacesBySlug = { acme: { id: "ws-acme", slug: "acme" } };

    const { queryClient } = renderLayout("acme");

    // Resolving the query is what would trigger the render-phase write.
    await waitFor(() => {
      expect(
        queryClient.getQueryState(["workspace-by-slug", "acme"])?.status,
      ).toBe("success");
    });
    expect(setCurrentWorkspace).not.toHaveBeenCalled();
  });

  it("lets only the routed layout write when both are mounted", async () => {
    state.pathname = "/globex/issues";
    state.workspacesBySlug = {
      acme: { id: "ws-acme", slug: "acme" },
      globex: { id: "ws-globex", slug: "globex" },
    };

    // The stale layout renders first, as it would during a transition the
    // router has not finished tearing down.
    const { queryClient } = renderTree(
      <>
        <WorkspaceLayout params={makeParams("acme")}>
          <div data-testid="stale-content" />
        </WorkspaceLayout>
        <WorkspaceLayout params={makeParams("globex")}>
          <div data-testid="routed-content" />
        </WorkspaceLayout>
      </>,
    );

    await waitFor(() => {
      expect(
        queryClient.getQueryState(["workspace-by-slug", "acme"])?.status,
      ).toBe("success");
      expect(
        queryClient.getQueryState(["workspace-by-slug", "globex"])?.status,
      ).toBe("success");
    });

    await waitFor(() => {
      expect(setCurrentWorkspace).toHaveBeenCalledWith("globex", "ws-globex");
    });
    // The regression: every call names the routed workspace. A single write
    // from the stale layout is one socket teardown and one empty mention list.
    for (const call of vi.mocked(setCurrentWorkspace).mock.calls) {
      expect(call).toEqual(["globex", "ws-globex"]);
    }
    const links = document.head.querySelectorAll('link[rel="search"]');
    expect(links).toHaveLength(1);
    expect(links[0]).toHaveAttribute("href", "/globex/opensearch.xml");
  });
});
