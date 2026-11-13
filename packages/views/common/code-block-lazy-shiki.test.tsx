import { render, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";

vi.mock("react-i18next", () => ({ useTranslation: () => ({ t: (f: unknown) => String(f) }) }));

// Shiki is ~750 KB decoded; it must not load with the page, only when code is rendered.
const shikiLoads = vi.hoisted(() => ({ count: 0 }));
vi.mock("shiki", () => {
  shikiLoads.count += 1;
  return {
    bundledLanguages: { javascript: () => null },
    codeToHtml: async (code: string) => `<pre class="shiki">${code}</pre>`,
  };
});

import { CodeBlock } from "@multica/ui/markdown/CodeBlock";

it("loads shiki when a code block renders, not when the module is imported", async () => {
  expect(shikiLoads.count).toBe(0);

  render(<CodeBlock code="const a = 1" language="js" mode="minimal" />);

  await waitFor(() => expect(document.querySelector(".shiki")).not.toBeNull());
  expect(shikiLoads.count).toBe(1);
  expect(screen.getByText("const a = 1")).toBeInTheDocument();
});
