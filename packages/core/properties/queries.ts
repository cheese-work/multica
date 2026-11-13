import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const propertyKeys = {
  all: (wsId: string) => ["properties", wsId] as const,
  list: (wsId: string) => [...propertyKeys.all(wsId), "list"] as const,
};

/**
 * One cached catalog serves every reader: the request always includes archived
 * definitions, and readers that want live ones only filter on read, so a page
 * mixing both costs one request.
 */
export function propertyListOptions(wsId: string, includeArchived = false) {
  return queryOptions({
    queryKey: propertyKeys.list(wsId),
    queryFn: () => api.listProperties(true),
    select: (data) =>
      includeArchived ? data.properties : data.properties.filter((p) => p.archived !== true),
  });
}
