export function useWorkspaceId(): string {
  return "ws-harness";
}

// Stubs for the data layer that settings-layout and wakeup-pickers import but the capture never calls.
// The real modules pull the auth store, which hangs headless Chrome at module init.
// Keyed by workspace and never stale, so views can seed a cache per workspace (read-only-member-view does).
const empty = (key: string, wsId?: string) => ({
  queryKey: ["harness", key, wsId ?? ""], queryFn: async () => [] as never[], staleTime: Infinity,
});
export const useCurrentWorkspace = () => null;
export const useWorkspacePaths = () => ({}) as Record<string, never>;
export const memberListOptions = (wsId?: string) => empty("members", wsId);
export const agentListOptions = (_wsId?: string) => empty("agents");
export const squadListOptions = (_wsId?: string) => empty("squads");
export const isAgentRuntimeBound = () => true;
export const ActorAvatar = () => null;
export const WorkspaceAvatar = () => null;
