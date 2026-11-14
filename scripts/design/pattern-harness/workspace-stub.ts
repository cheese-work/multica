import { createElement } from "react";
import { ActorAvatar as ActorAvatarBase } from "@multica/ui/components/common/actor-avatar";
import type { AvatarSize } from "@multica/ui/lib/avatar-size";
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
// Initials circle from the real ui avatar, so avatar-only views (quick-agent-bar) show their real shape (CHE-5536).
export const ActorAvatar = ({ actorId = "", actorType, size, className }: { actorId?: string; actorType?: string; size?: AvatarSize; className?: string }) =>
  createElement(ActorAvatarBase, { name: actorId, initials: actorId.replace(/^(agent|x99)-/, "").slice(0, 2).toUpperCase(), isAgent: actorType === "agent", size, className });
export const WorkspaceAvatar = () => null;
// @multica/core/agents is aliased here too (create-shell); settings-page pulls profile cards that import these two.
export const useAgentPresenceDetail = () => null;
export const agentTaskSnapshotOptions = (_wsId?: string, _id?: string) => empty("agent-task-snapshot");
export const agentRunCounts30dOptions = (_wsId?: string) => empty("agent-run-counts");
export const workspaceListOptions = () => empty("workspaces");
export const useWorkspaceSlug = () => "harness";
// Pulled in by the real editor/hover-card modules the S0b raw-element pages render; never called in a capture.
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
