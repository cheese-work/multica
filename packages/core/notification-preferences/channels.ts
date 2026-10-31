import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { useWorkspaceId } from "../hooks";
import { useRequiredWorkspaceSlug } from "../paths";
import type { NotificationChannelsResponse, UpdateNotificationChannelRequest } from "../types";

export const notificationChannelKeys = {
  all: (wsId: string) => ["notification-channels", wsId] as const,
};

export function notificationChannelOptions(wsId: string, workspaceSlug?: string) {
  return queryOptions({
    queryKey: notificationChannelKeys.all(wsId),
    queryFn: () => api.getNotificationChannels(workspaceSlug),
  });
}

/**
 * One switch per call. The server answers with every switch, so the cache takes
 * the response instead of a guess: a failed save leaves the switch where it was.
 */
export function useUpdateNotificationChannel() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  const workspaceSlug = useRequiredWorkspaceSlug();
  return useMutation({
    mutationFn: (req: UpdateNotificationChannelRequest) => api.updateNotificationChannel(req, workspaceSlug),
    onSuccess: (data: NotificationChannelsResponse) => {
      qc.setQueryData(notificationChannelKeys.all(wsId), data);
    },
  });
}
