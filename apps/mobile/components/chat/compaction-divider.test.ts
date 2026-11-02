// @vitest-environment node
import { Pressable } from "react-native";
import { describe, expect, it, vi } from "vitest";
import { CompactionDivider } from "./compaction-divider";

vi.mock("react-native", () => ({ Pressable: "Pressable", View: "View" }));
vi.mock("@/components/ui/text", () => ({ Text: "Text" }));
vi.mock("@/components/ui/collapsible", () => ({
  Collapsible: "Collapsible",
  CollapsibleContent: "CollapsibleContent",
  CollapsibleTrigger: "CollapsibleTrigger",
}));
vi.mock("@/lib/markdown", () => ({ Markdown: "Markdown" }));
vi.mock("@/lib/i18n", () => ({
  useT: () => ({ t: (key: string) => key }),
}));

const message = {
  id: "m1",
  content: "summary",
  attachments: [],
  created_at: "2026-10-29T18:00:00Z",
} as never;

// Inspect props without pretending to render native UI. The Collapsible
// primitive slots `onPress` onto its `asChild` child, so that child must be
// pressable: a native `View` ignores `onPress` and the summary never opens.
describe("CompactionDivider", () => {
  it("uses a pressable trigger so the summary can expand", () => {
    const tree = CompactionDivider({ message, isSelecting: false });
    const trigger = tree.props.children[0];
    expect(trigger.props.asChild).toBe(true);
    const child = trigger.props.children;
    expect(child.type).toBe(Pressable);
    expect(child.props.accessibilityRole).toBe("button");
    expect(child.props.accessibilityLabel).toBeTruthy();
  });
});
