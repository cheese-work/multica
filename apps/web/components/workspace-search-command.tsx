"use client";

import { useEffect } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useWorkspacePaths } from "@multica/core/paths";
import { SearchCommand, useSearchStore } from "@multica/views/search";

export function WorkspaceSearchCommand() {
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const router = useRouter();
  const workspacePaths = useWorkspacePaths();
  const isSearchRoute = pathname === workspacePaths.search();
  const initialQuery = isSearchRoute ? (searchParams.get("q") ?? "") : undefined;
  const workspaceRoot = workspacePaths.root();

  useEffect(() => {
    if (!isSearchRoute) return;
    const unsubscribe = useSearchStore.subscribe((state, previous) => {
      if (previous.open && !state.open) router.replace(workspaceRoot);
    });
    useSearchStore.getState().setOpen(true);
    return () => {
      unsubscribe();
      useSearchStore.getState().setOpen(false);
    };
  }, [isSearchRoute, initialQuery, router, workspaceRoot]);

  return <SearchCommand initialQuery={initialQuery} />;
}
