export function useWorkspaceId(): string {
  return "ws-harness";
}

// Stubs for the data layer that settings-layout and wakeup-pickers import but the capture never calls.
// The real modules pull the auth store, which hangs headless Chrome at module init.
const empty = (key: string) => ({ queryKey: ["harness", key], queryFn: async () => [] as never[] });
export const useCurrentWorkspace = () => null;
export const memberListOptions = (_wsId?: string) => empty("members");
export const agentListOptions = (_wsId?: string) => empty("agents");
export const squadListOptions = (_wsId?: string) => empty("squads");
export const isAgentRuntimeBound = () => true;
export const ActorAvatar = () => null;
export const WorkspaceAvatar = () => null;
export const workspaceListOptions = () => empty("workspaces");
export const useWorkspaceSlug = () => "harness";
// Pulled in by the real editor/hover-card modules the S0b raw-element pages render; never called in a capture.
export const agentTaskSnapshotOptions = (_wsId?: string) => empty("agent-task-snapshot");
export const isGlobalPath = () => false;
export const isKnownWorkspacePath = () => false;
export const workspaceSlugForPath = () => null;
export const mediaPath = (_slug?: string, p?: string) => p ?? "";
export const agentPresetCompletionsOptions = (_wsId?: string) => empty("agent-preset-completions");
export const filterPresetCompletions = <T,>(items: T[]) => items;
export const workspaceKeys = {
  members: (wsId: string) => ["harness", "members", wsId],
  agents: (wsId: string) => ["harness", "agents", wsId],
};
export const paths = new Proxy({}, { get: () => () => "/harness" }) as Record<string, (...a: string[]) => string>;
export const useWorkspacePaths = () => paths;
