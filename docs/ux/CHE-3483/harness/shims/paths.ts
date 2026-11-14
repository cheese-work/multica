export * from "../../../../../packages/core/paths/index";

export function useCurrentWorkspace() {
  return { id: "ws-capture", slug: "capture", name: "Capture fixture" } as never;
}
