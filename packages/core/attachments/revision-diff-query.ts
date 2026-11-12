import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { api } from "../api";

export const attachmentRevisionDiffKeys = {
  diff: (id: string, from: number, to: number, full: boolean) =>
    ["attachment-revision-diff", id, from, to, full] as const,
};

/** Diff of two revisions of one attachment. Revisions never change, so it caches for good. */
export function useAttachmentRevisionDiff(
  id: string,
  from: number,
  to: number,
  full: boolean,
  enabled = true,
) {
  return useQuery({
    queryKey: attachmentRevisionDiffKeys.diff(id, from, to, full),
    queryFn: () => api.getAttachmentRevisionDiff(id, from, to, full),
    staleTime: Infinity,
    // Toggling full/changes keeps the shown diff until the other one arrives.
    placeholderData: keepPreviousData,
    enabled,
  });
}
