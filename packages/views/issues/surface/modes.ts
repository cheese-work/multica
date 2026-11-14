import type { IssueSurfaceMode } from "./types";

// The mode allowlist of each surface that hosts Swimlane 2.0 (CHE-4312). One list per page, so a surface test can render the real list.
export const WORKSPACE_ISSUE_MODES: IssueSurfaceMode[] = ["board", "list", "table", "swimlane", "swimlane2"];
export const MY_ISSUE_MODES: IssueSurfaceMode[] = ["board", "list", "table", "swimlane", "swimlane2"];
export const PROJECT_ISSUE_MODES: IssueSurfaceMode[] = ["board", "list", "table", "swimlane", "gantt", "swimlane2"];
