"use client";

import { useEffect } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useCurrentWorkspace, useWorkspacePaths } from "@multica/core/paths";
import { SearchCommand, useSearchStore } from "@multica/views/search";

export function WorkspaceSearchCommand() {
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const router = useRouter();
  const workspacePaths = useWorkspacePaths();
  const workspace = useCurrentWorkspace();
  const isSearchRoute = pathname === workspacePaths.search();
  const initialQuery = isSearchRoute ? (searchParams.get("q") ?? "") : undefined;
  const workspaceRoot = workspacePaths.root();
  const leadingIdentifier = initialQuery?.trim().match(/^([a-z0-9]+)-(\d+)(?:\s|$)/i);
  const waitingForWorkspace = Boolean(leadingIdentifier && !workspace);
  const prefix = workspace?.issue_prefix || workspace?.name.replace(/[^a-z]/gi, "").slice(0, 3).toUpperCase() || "WS";
  const issueTarget = leadingIdentifier && prefix && leadingIdentifier[1]?.toUpperCase() === prefix.toUpperCase()
    ? workspacePaths.issueDetail(`${prefix}-${leadingIdentifier[2]}`)
    : null;

  useEffect(() => {
    if (!isSearchRoute) return;
    if (waitingForWorkspace) return;
    if (issueTarget) {
      useSearchStore.getState().setOpen(false);
      router.replace(issueTarget);
      return;
    }
    const unsubscribe = useSearchStore.subscribe((state, previous) => {
      if (previous.open && !state.open) router.replace(workspaceRoot);
    });
    useSearchStore.getState().setOpen(true);
    return () => {
      unsubscribe();
      useSearchStore.getState().setOpen(false);
    };
  }, [isSearchRoute, initialQuery, router, workspaceRoot, issueTarget, waitingForWorkspace]);

  return <SearchCommand initialQuery={initialQuery} />;
}
