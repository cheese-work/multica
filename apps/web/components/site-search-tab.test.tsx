import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { I18nProvider } from "@multica/core/i18n/react";
import settings from "@multica/views/locales/en/settings.json";

const state = vi.hoisted(() => ({ copyText: vi.fn() }));
vi.mock("@multica/ui/lib/clipboard", () => ({ copyText: state.copyText }));
vi.mock("@multica/views/navigation", () => ({
  useNavigation: () => ({ getShareableUrl: (path: string) => `https://app.multica.test${path}` }),
}));

import { SiteSearchTab } from "./site-search-tab";

function renderTab() {
  return render(
    <I18nProvider locale="en" resources={{ en: { settings } }}>
      <WorkspaceSlugProvider slug="acme"><SiteSearchTab /></WorkspaceSlugProvider>
    </I18nProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  state.copyText.mockResolvedValue(true);
});

describe("site search settings", () => {
  it("shows the current-origin workspace template and Brave hint, and copies literal %s", async () => {
    const user = userEvent.setup();
    renderTab();
    expect(await screen.findByText("https://app.multica.test/acme/search?q=%s")).toBeInTheDocument();
    expect(screen.getByText("Add in brave://settings/search → Site search → Add")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Copy" }));
    expect(state.copyText).toHaveBeenCalledExactlyOnceWith("https://app.multica.test/acme/search?q=%s");
    expect(screen.getByRole("status")).toHaveTextContent("Search URL copied");
  });

  it("reports clipboard failure without announcing success", async () => {
    state.copyText.mockResolvedValue(false);
    const user = userEvent.setup();
    renderTab();
    await user.click(screen.getByRole("button", { name: "Copy" }));
    expect(screen.getByRole("status")).toHaveTextContent("Could not copy search URL");
    expect(screen.queryByText("Search URL copied")).not.toBeInTheDocument();
  });
});
