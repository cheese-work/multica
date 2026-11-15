import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

// CHE-3019: the `issues/components` and `projects/components` barrels also
// export the detail views (editor, comments, ...). A file that reaches them
// for one icon drags the whole detail graph into every route's startup
// chunk, which costs ~1.5 s of script evaluation at 4x CPU throttle.
const BARREL = /from\s+["'](?:\.\.\/)+(?:issues|projects)\/components["']|from\s+["']@multica\/views\/(?:issues|projects)\/components["']/;

const STARTUP_FILES = [
  "layout/tab-presentation.tsx",
  "search/result-rows.tsx",
  "inbox/components/inbox-list-item.tsx",
  "inbox/components/inbox-detail-label.tsx",
  "../../apps/web/app/[workspaceSlug]/(dashboard)/issues/page.tsx",
  "../../apps/web/app/[workspaceSlug]/(dashboard)/projects/page.tsx",
  "../../apps/web/features/landing/components/features-section.tsx",
];

describe("startup path imports", () => {
  it.each(STARTUP_FILES)("%s avoids the detail-view barrels", (file) => {
    const source = readFileSync(resolve(__dirname, file), "utf8");
    expect(source).not.toMatch(BARREL);
  });
});

describe("modal registry", () => {
  it("loads each modal on first open, not at startup", () => {
    const source = readFileSync(resolve(__dirname, "modals/registry.tsx"), "utf8");
    const eager = source.match(/^import \{[^}]+\} from "\.\/(?!issue-limit-upgrade-dialog)[^"]+";$/gm) ?? [];
    expect(eager).toEqual([]);
  });
});

describe("floating chat", () => {
  it("loads the chat window after first paint, not at startup", () => {
    const source = readFileSync(resolve(__dirname, "chat/floating-chat.tsx"), "utf8");
    expect(source).not.toMatch(/^import \{ ChatWindow \}/m);
  });
});

describe("dashboard layout", () => {
  it("imports the source-backfill modal directly, not through the onboarding barrel", () => {
    const source = readFileSync(resolve(__dirname, "layout/dashboard-layout.tsx"), "utf8");
    expect(source).not.toMatch(/from\s+["']\.\.\/onboarding["']/);
  });
});

// Next warns (INEFFECTIVE_DYNAMIC_IMPORT, which fails the warning budget) when
// a module is imported dynamically in one place and statically in another.
// Every importer of the autopilot form goes through its lazy wrapper.
describe("autopilot dialog", () => {
  it.each([
    "search/host-dialogs.tsx",
    "autopilots/components/autopilots-page.tsx",
    "autopilots/components/autopilot-detail-page.tsx",
  ])("%s does not import the dialog statically", (file) => {
    const source = readFileSync(resolve(__dirname, file), "utf8");
    expect(source).not.toMatch(/from\s+["'][^"']*\/autopilot-dialog["']/);
  });
});
