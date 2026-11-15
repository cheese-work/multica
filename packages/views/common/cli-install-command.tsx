"use client";

import { useEffect, useState, type ReactNode } from "react";
import { api } from "@multica/core/api";
import { useConfigStore } from "@multica/core/config";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@multica/ui/components/ui/tabs";

/**
 * The Multica CLI install command, one per platform family.
 *
 * Three surfaces render install instructions — the runtimes "add computer"
 * dialog, onboarding's CLI card, and the landing download page. They read the
 * command from here instead of each hardcoding a string.
 *
 * The script comes from the server the user is connecting to
 * (`GET /api/daemon/install.sh`, CHE-2641), never from upstream: it installs
 * the server's own build. The server builds Linux and macOS only, so there is
 * no Windows entry.
 */
export function cliInstallCommand(serverUrl: string): string {
  return `curl -fsSL ${serverUrl}/api/daemon/install.sh | bash`;
}

const trimUrl = (url: string) => url.trim().replace(/\/+$/, "");

/** Placeholder shown until the page origin is known (server render). */
const SERVER_URL_PLACEHOLDER = "https://<your-multica-server>";

function configuredApiBaseUrl(): string {
  try {
    return api.getBaseUrl();
  } catch {
    return ""; // no ApiClient on this surface (e.g. the landing page)
  }
}

/**
 * The server to install from: the operator's configured server URL, else the
 * API client's base URL, else the page origin.
 */
function useInstallServerUrl(): string {
  const configured = useConfigStore((s) => s.daemonServerUrl);
  const [origin, setOrigin] = useState("");
  useEffect(() => setOrigin(window.location.origin), []);
  return (
    trimUrl(configured) ||
    trimUrl(configuredApiBaseUrl()) ||
    trimUrl(origin) ||
    SERVER_URL_PLACEHOLDER
  );
}

export type CliInstallPlatform = "macosLinux";

/** Tab order. */
const PLATFORMS = [
  "macosLinux",
] as const satisfies readonly CliInstallPlatform[];

export interface CliInstallCommandLabels {
  /** Accessible name for the platform switch. */
  group: string;
  macosLinux: string;
}

/**
 * Platform switch for the install command, built on the shared Tabs primitive
 * so tab semantics and arrow-key navigation come for free.
 *
 * The switch is the only shared UI: `children` receives the selected command
 * and renders it, so each surface keeps its own command row — the product
 * surfaces their token-styled row with an icon copy button, the landing page
 * its marketing palette with a spelled-out one.
 */
export function CliInstallCommand({
  labels,
  classNames,
  children,
}: {
  labels: CliInstallCommandLabels;
  /** Palette overrides for surfaces that do not use the product tokens. */
  classNames?: { list?: string; trigger?: string };
  children: (command: string) => ReactNode;
}) {
  const command = cliInstallCommand(useInstallServerUrl());
  return (
    <Tabs defaultValue={PLATFORMS[0]}>
      <TabsList
        aria-label={labels.group}
        activateOnFocus
        className={classNames?.list}
      >
        {PLATFORMS.map((platform) => (
          <TabsTrigger
            key={platform}
            value={platform}
            className={classNames?.trigger}
          >
            {labels[platform]}
          </TabsTrigger>
        ))}
      </TabsList>
      {PLATFORMS.map((platform) => (
        <TabsContent key={platform} value={platform}>
          {children(command)}
        </TabsContent>
      ))}
    </Tabs>
  );
}
