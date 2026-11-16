// Harness stand-in for @multica/core/paths. The real barrel pulls `resolve.ts`
// (auth store), which hangs headless Chrome at module init. Pure modules are
// re-exported; the hooks are fixed to the harness workspace.
import type { ReactNode } from "react";
import { paths } from "../../../packages/core/paths/paths";

export * from "../../../packages/core/paths/paths";
export * from "../../../packages/core/paths/reserved-slugs";
export * from "../../../packages/core/paths/route-icons";
export * from "../../../packages/core/paths/spaces";
export * from "../../../packages/core/paths/tab-subject";
export * from "../../../packages/core/paths/tab-presentation";
export * from "../../../packages/core/paths/workspace-slugs";
export * from "../../../packages/core/paths/media";

export const useCurrentWorkspace = () => ({ id: "ws-harness", name: "Acme", slug: "acme", avatar_url: null });
export const useWorkspaceSlug = () => "acme";
export const useRequiredWorkspaceSlug = () => "acme";
export const useWorkspacePaths = () => paths.workspace("acme");
export const WorkspaceSlugProvider = ({ children }: { slug?: string; children?: ReactNode }) => children;
export const useHasOnboarded = () => true;
export const resolvePostAuthDestination = () => "/acme/issues";
