// @vitest-environment jsdom

/**
 * Fork feature: filter issues by their own workflow override. Driven against
 * the real Base UI menu primitives, like the property-filter test.
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createStore } from "zustand/vanilla";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import {
  type IssueViewState,
  viewStoreSlice,
} from "@multica/core/issues/stores/view-store";
import { ViewStoreProvider } from "@multica/core/issues/stores/view-store-context";
import { renderWithI18n } from "../../test/i18n";
import { IssueFilterMenu } from "./issues-header";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/issue-workflows", () => ({
  useIssueWorkflows: () => ({
    workflows: [
      { id: "wf-release", name: "Release flow" },
      { id: "wf-triage", name: "Triage flow" },
    ],
    isLoaded: true,
    isError: false,
    retry: () => {},
  }),
}));

function renderFilterMenu(workflowFilters: string[] = []) {
  setApiInstance({
    listIssueStatuses: async () => ({ statuses: [], categories: [], total: 0 }),
    listProperties: async () => ({ properties: [] }),
  } as unknown as ApiClient);
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  });
  const store = createStore<IssueViewState>()(viewStoreSlice);
  store.setState({ workflowFilters });
  renderWithI18n(
    <QueryClientProvider client={qc}>
      <ViewStoreProvider store={store}>
        <IssueFilterMenu trigger={<button type="button">Filter</button>} />
      </ViewStoreProvider>
    </QueryClientProvider>,
  );
  return store;
}

afterEach(() => {
  cleanup();
  document.body.innerHTML = "";
  vi.restoreAllMocks();
});

describe("IssueFilterMenu workflow filter", () => {
  it("lists the workspace workflows and toggles the selection in the store", async () => {
    const store = renderFilterMenu();
    fireEvent.click(screen.getByRole("button", { name: "Filter" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: /Workflow/ }));
    fireEvent.click(await screen.findByRole("menuitemcheckbox", { name: "Release flow" }));
    expect(store.getState().workflowFilters).toEqual(["wf-release"]);
    fireEvent.click(screen.getByRole("menuitemcheckbox", { name: "Release flow" }));
    expect(store.getState().workflowFilters).toEqual([]);
  });

  it("shows the selected count on the Workflow entry", async () => {
    renderFilterMenu(["wf-release", "wf-triage"]);
    fireEvent.click(screen.getByRole("button", { name: "Filter" }));
    const entry = await screen.findByRole("menuitem", { name: /Workflow/ });
    expect(entry).toHaveTextContent("2");
  });
});
