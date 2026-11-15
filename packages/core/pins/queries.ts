import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const pinKeys = {
  workspace: (wsId: string) => ["pins", wsId] as const,
  all: (wsId: string, userId: string) => [...pinKeys.workspace(wsId), userId] as const,
  list: (wsId: string, userId: string) => [...pinKeys.all(wsId, userId), "list"] as const,
};

export function pinListOptions(wsId: string, userId: string) {
  return queryOptions({
    queryKey: pinKeys.list(wsId, userId),
    queryFn: () => api.listPins(),
  });
}
