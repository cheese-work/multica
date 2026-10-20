"use client";

import { useCallback } from "react";
import { toast } from "sonner";
import { useT } from "../i18n";

/**
 * Tells the person why a status change was held back and offers to reload the
 * workflow and project lists. A catalog that has not loaded, failed, or loaded
 * without the issue's workflow cannot say whether the change hands the issue
 * off, so the write waits instead of risking an unconfirmed handoff.
 */
export function useNotifyCatalogUnresolved(): (retry: () => void) => void {
  const { t } = useT("issues");
  return useCallback(
    (retry) =>
      toast.error(t(($) => $.workflows.catalog_unresolved), {
        action: { label: t(($) => $.workflows.catalog_retry), onClick: retry },
      }),
    [t],
  );
}
