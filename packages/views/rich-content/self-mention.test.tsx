import { act, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { createAuthStore, registerAuthStore } from "@multica/core/auth";
import type { ApiClient } from "@multica/core/api";
import type { User } from "@multica/core/types";
import { RichContent } from "./rich-content";

vi.mock("../i18n", async () => {
  const editor = (await import("../locales/en/editor.json")).default;
  return {
    useT: () => ({
      t: (select: (bundle: typeof editor) => string) => select(editor),
    }),
  };
});

vi.mock("../editor/link-hover-card", () => ({
  useLinkHover: () => ({}),
  LinkHoverCard: () => null,
}));

const VIEWER_ID = "f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff";
const OTHER_ID = "8f14e45f-ceea-4d0e-a1a2-9b1c0d3e4f5a";
const CONTENT = [
  `[@ChEeSe](mention://member/${VIEWER_ID})`,
  `[@Other](mention://member/${OTHER_ID})`,
  `[@Agent](mention://agent/${VIEWER_ID})`,
  "[@all](mention://all/all)",
].join(" ");

let authStore: ReturnType<typeof createAuthStore>;

beforeEach(() => {
  authStore = createAuthStore({
    api: {} as ApiClient,
    storage: {
      getItem: () => null,
      setItem: () => {},
      removeItem: () => {},
    },
  });
  authStore.setState({ user: { id: VIEWER_ID } as User });
  registerAuthStore(authStore);
});

describe("RichContent self-member mentions", () => {
  it.each(["compact", "document"] as const)(
    "highlights only the viewer's member mention in %s content without changing case",
    (density) => {
      render(<RichContent content={CONTENT} density={density} />);

      expect(screen.getByText("@ChEeSe")).toHaveClass("mention", "mention-self");
      expect(screen.getByText("@ChEeSe")).toHaveTextContent(/^@ChEeSe$/);
      for (const label of ["@Other", "@Agent", "@all"]) {
        expect(screen.getByText(label)).toHaveAttribute("class", "mention");
      }
    },
  );

  it("does not highlight member mentions without a current user", () => {
    authStore.setState({ user: null });
    const { container } = render(<RichContent content={CONTENT} />);

    expect(container.querySelectorAll(".mention")).toHaveLength(4);
    expect(container.querySelector(".mention-self")).toBeNull();
  });

  it("updates the self variant when the viewer changes or logs out", () => {
    render(<RichContent content={CONTENT} />);

    act(() => authStore.setState({ user: { id: OTHER_ID } as User }));
    expect(screen.getByText("@ChEeSe")).toHaveAttribute("class", "mention");
    expect(screen.getByText("@Other")).toHaveClass("mention-self");

    act(() => authStore.setState({ user: null }));
    expect(screen.getByText("@Other")).toHaveAttribute("class", "mention");
  });
});
