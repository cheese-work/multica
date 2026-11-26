"use client";

import { useQuery } from "@tanstack/react-query";
import type { Agent, MemberWithUser } from "@multica/core/types";
import { agentBackgroundUseOptions } from "@multica/core/workspace/queries";
import { usesInvocationMembers } from "@multica/core/agents";
import { usePaidFeatureAccess } from "@multica/core/billing";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  SettingsCard,
  SettingsSection,
} from "../../settings/components/settings-layout";
import { useT } from "../../i18n";
import { AccessPicker } from "./inspector/access-picker";

export function AgentAccessSettings({
  agent,
  members,
  currentUserId,
  onDirtyChange,
  onUpdate,
}: {
  agent: Agent;
  members: MemberWithUser[];
  currentUserId: string | null;
  onDirtyChange?: (dirty: boolean) => void;
  onUpdate: (id: string, data: Record<string, unknown>) => Promise<void>;
}) {
  const { t } = useT("agents");
  const { data: backgroundUse = [] } = useQuery(
    agentBackgroundUseOptions(agent.workspace_id, agent.id),
  );
  const isWorkspaceBackgroundAgent = backgroundUse.some(
    (u) => u.scope === "workspace",
  );
  const canEdit = currentUserId !== null && agent.owner_id === currentUserId;
  const wsId = useWorkspaceId();
  const membersAccess = usePaidFeatureAccess(wsId, "agent_invocation_members", {
    hasExistingConfig: usesInvocationMembers(agent.permission_mode, agent.invocation_targets),
  });

  return (
    <SettingsSection
      title={t(($) => $.access.section_title)}
    >
      <SettingsCard>
        <AccessPicker
          permissionMode={agent.permission_mode}
          invocationTargets={agent.invocation_targets}
          visibility={agent.visibility}
          members={members}
          ownerId={agent.owner_id}
          canEdit={canEdit}
          hasComposioAllowlist={
            (agent.composio_toolkit_allowlist ?? []).length > 0
          }
          onDirtyChange={onDirtyChange}
          membersAccess={membersAccess}
          onChange={(next) => onUpdate(agent.id, next)}
        />
        {isWorkspaceBackgroundAgent && (
          <p className="mt-3 text-caption text-muted-foreground">
            {canEdit
              ? t(($) => $.background_use.access_note)
              : t(($) => $.background_use.access_note_readonly)}
          </p>
        )}
      </SettingsCard>
    </SettingsSection>
  );
}
