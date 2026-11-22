import { infiniteQueryOptions } from "@tanstack/react-query";
import { api } from "../api";
import type { AuditEventFilters } from "../types/audit";

export const AUDIT_PAGE_SIZE = 50;

/** The cursor is part of the request, never of the key: it is the page param. */
export type AuditLogFilters = Omit<AuditEventFilters, "cursor" | "limit">;

export const auditKeys = {
  all: (wsId: string) => ["audit-events", wsId] as const,
  list: (wsId: string, filters: AuditLogFilters) => ["audit-events", wsId, filters] as const,
};

/** Audit rows newest first, one keyset page at a time. A filter change is a new key, so it starts at page one. */
export function auditEventsOptions(wsId: string, filters: AuditLogFilters) {
  return infiniteQueryOptions({
    queryKey: auditKeys.list(wsId, filters),
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      api.listAuditEvents(wsId, { ...filters, limit: AUDIT_PAGE_SIZE, ...(pageParam ? { cursor: pageParam } : {}) }),
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    enabled: !!wsId,
  });
}
