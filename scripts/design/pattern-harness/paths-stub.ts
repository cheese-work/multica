// The paths barrel pulls resolve.ts and the auth store, which hang headless Chrome at module init.
// Re-export the leaf modules the components need.
export * from "../../../packages/core/paths/hooks";
export { paths } from "../../../packages/core/paths/paths";
export { mediaPath } from "../../../packages/core/paths/media";
export { isKnownWorkspacePath, workspaceSlugForPath } from "../../../packages/core/paths/workspace-slugs";
export { isGlobalPath } from "../../../packages/core/paths/paths";
