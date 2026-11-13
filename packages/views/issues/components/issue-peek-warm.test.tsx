import { fireEvent, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { renderWithI18n } from "../../test/i18n";
import { NavigationProvider, type NavigationAdapter } from "../../navigation";
import { IssuePeekHost } from "./issue-peek";

vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ issueDetail: (id: string) => `/acme/issues/${id}` }),
}));

// Its own file: a loaded mock module stays cached for the rest of a file, so the
// first load is only observable once per file.
const detailLoads = vi.hoisted(() => ({ count: 0 }));
vi.mock("./issue-detail", () => {
  detailLoads.count += 1;
  return { IssueDetail: () => null };
});

const navigation: NavigationAdapter = {
  push: vi.fn(),
  replace: vi.fn(),
  back: vi.fn(),
  pathname: "/acme/issues",
  searchParams: new URLSearchParams(),
  hash: "",
  getShareableUrl: (path) => `https://app.example${path}`,
};

it("loads the issue detail when a card takes keyboard focus, not before", async () => {
  renderWithI18n(
    <NavigationProvider value={navigation}>
      <IssuePeekHost>
        <button type="button">toolbar</button>
        <button type="button" data-peek-target="i-1">card i-1</button>
      </IssuePeekHost>
    </NavigationProvider>,
  );

  fireEvent.focus(screen.getByText("toolbar"));
  expect(detailLoads.count).toBe(0);

  fireEvent.focus(screen.getByText("card i-1"));
  await waitFor(() => expect(detailLoads.count).toBe(1));
});
