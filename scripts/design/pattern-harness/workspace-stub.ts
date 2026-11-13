export function useWorkspaceId(): string {
  return "ws-harness";
}

// Stubs for the data layer that settings-layout and wakeup-pickers import but the capture never calls.
// The real modules pull the auth store, which hangs headless Chrome at module init.
const empty = (key: string) => ({ queryKey: ["harness", key], queryFn: async () => [] as never[] });
export const useCurrentWorkspace = () => null;
export const useWorkspacePaths = () => ({}) as Record<string, never>;
export const memberListOptions = (_wsId?: string) => empty("members");
export const agentListOptions = (_wsId?: string) => empty("agents");
export const squadListOptions = (_wsId?: string) => empty("squads");
export const isAgentRuntimeBound = () => true;
export const ActorAvatar = () => null;
export const WorkspaceAvatar = () => null;
