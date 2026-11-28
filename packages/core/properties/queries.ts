import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const propertyKeys = {
  all: (wsId: string) => ["properties", wsId] as const,
  list: (wsId: string) => [...propertyKeys.all(wsId), "list"] as const,
  /** The settings catalog with per-definition usage counts. Value writes only
   *  move counts, so they invalidate this key and leave `list` alone. */
  withUsage: (wsId: string) => [...propertyKeys.all(wsId), "with-usage"] as const,
};

/**
 * One cached catalog serves every reader: the request always includes archived
 * definitions, and readers that want live ones only filter on read, so a page
 * mixing both costs one request. `usage_count` is always 0 here; use
 * `propertyUsageListOptions` where counts are shown.
 */
export function propertyListOptions(wsId: string, includeArchived = false) {
  return queryOptions({
    queryKey: propertyKeys.list(wsId),
    queryFn: () => api.listProperties(true),
    select: (data) =>
      includeArchived ? data.properties : data.properties.filter((p) => p.archived !== true),
  });
}

/** Every definition, archived included, with real usage counts. */
export function propertyUsageListOptions(wsId: string) {
  return queryOptions({
    queryKey: propertyKeys.withUsage(wsId),
    queryFn: () => api.listProperties(true, { includeUsage: true }),
    select: (data) => data.properties,
  });
}
