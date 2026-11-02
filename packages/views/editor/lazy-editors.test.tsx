import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";

const loaded = vi.hoisted(() => ({ content: 0, title: 0 }));

vi.mock("./content-editor", async () => {
  loaded.content++;
  const { forwardRef } = await import("react");
  return {
    ContentEditor: forwardRef<unknown, { defaultValue?: string }>((p, _ref) => (
      <div data-testid="real-content">{p.defaultValue}</div>
    )),
  };
});
vi.mock("./title-editor", async () => {
  loaded.title++;
  const { forwardRef } = await import("react");
  return {
    TitleEditor: forwardRef<unknown, { defaultValue?: string }>((p, _ref) => (
      <div data-testid="real-title">{p.defaultValue}</div>
    )),
  };
});

describe("lazy editors", { timeout: 30_000 }, () => {
  it("does not load the tiptap editors until one renders", async () => {
    const { ContentEditor, TitleEditor } = await import("./lazy-editors");
    expect(loaded).toEqual({ content: 0, title: 0 });

    render(<ContentEditor defaultValue="body" />);
    expect(await screen.findByTestId("real-content", {}, { timeout: 15_000 })).toHaveTextContent("body");
    expect(loaded).toEqual({ content: 1, title: 0 });

    render(<TitleEditor defaultValue="head" />);
    expect(await screen.findByTestId("real-title", {}, { timeout: 15_000 })).toHaveTextContent("head");
    expect(loaded).toEqual({ content: 1, title: 1 });
  });
});
