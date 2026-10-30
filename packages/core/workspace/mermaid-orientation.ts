"use client";

import { use } from "react";
import { QueryClient, QueryClientContext, useQuery } from "@tanstack/react-query";
import { z } from "zod";
import { parseWithFallback } from "../api/schema";
import { useWorkspaceSlug } from "../paths/hooks";
import { workspaceListOptions } from "./queries";

/** `auto` flips a diagram to fit its container's width; `author` draws it as written. */
export type MermaidOrientationSetting = "auto" | "author";

const DEFAULT_SETTING: MermaidOrientationSetting = "auto";
const SettingSchema = z.enum(["auto", "author"]).optional();

/** Reads workspace.settings.mermaid_orientation; a missing or unknown value is `auto`. */
export function readMermaidOrientationSetting(
  settings: Record<string, unknown> | undefined,
): MermaidOrientationSetting {
  return (
    parseWithFallback<MermaidOrientationSetting | undefined>(
      settings?.mermaid_orientation,
      SettingSchema,
      DEFAULT_SETTING,
      { endpoint: "workspace.settings.mermaid_orientation" },
    ) ?? DEFAULT_SETTING
  );
}

// Diagrams also render where no workspace or QueryClient exists (the
// standalone media page, SSR). `useQuery` needs some client, so those callers
// get a disabled query on this one, built on first use so importing this
// module touches no react-query export.
let detachedClient: QueryClient | undefined;

/** The current workspace's setting, or `auto` outside a workspace route. */
export function useMermaidOrientationSetting(): MermaidOrientationSetting {
  const slug = useWorkspaceSlug();
  const client = use(QueryClientContext) ?? (detachedClient ??= new QueryClient());
  const { data } = useQuery({ ...workspaceListOptions(), enabled: !!slug }, client);
  const workspace = slug ? data?.find((w) => w.slug === slug) : undefined;
  return readMermaidOrientationSetting(workspace?.settings);
}
