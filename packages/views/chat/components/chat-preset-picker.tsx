"use client";

import { useQuery } from "@tanstack/react-query";
import { SlidersHorizontal } from "@multica/ui/icons";
import { IconButton } from "@multica/ui/components/common/icon-button";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
} from "@multica/ui/components/ui/dropdown-menu";
import { agentPresetCompletionsOptions } from "@multica/core/agents";
import { useWorkspaceId } from "@multica/core/hooks";
import { useT } from "../../i18n";

/**
 * Chooses the preset this DM uses. There is no PATCH for it: a pick is sent as
 * the `/preset <reset|base|name>` command, which the server answers.
 * `current` is the session's preset_name: null = inherit the agent default.
 */
export function ChatPresetPicker({
  agentId,
  current,
  onSelect,
}: {
  agentId: string;
  current: string | null;
  onSelect: (name: string) => void;
}) {
  const { t } = useT("chat");
  const wsId = useWorkspaceId();
  const { data: completions = [] } = useQuery(agentPresetCompletionsOptions(wsId, agentId));
  // Completions lead with the `base` and `reset` keywords; the menu words them itself.
  const names = completions.filter((n) => n !== "base" && n !== "reset");
  if (names.length === 0 && current === null) return null;

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <IconButton
            label={t(($) => $.header.preset.label)}
            tooltipSide="bottom"
            className="text-muted-foreground"
          />
        }
      >
        <SlidersHorizontal className="h-4 w-4" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuRadioGroup
          value={current ?? "reset"}
          onValueChange={(value) => onSelect(value as string)}
        >
          <DropdownMenuRadioItem value="reset">{t(($) => $.header.preset.inherit)}</DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="base">{t(($) => $.header.preset.base)}</DropdownMenuRadioItem>
          {names.map((name) => (
            <DropdownMenuRadioItem key={name} value={name}>
              {name}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
