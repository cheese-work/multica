"use client";

import { useRef } from "react";
import {
  ArrowUpRight,
  BookOpen,
  CircleHelp,
  Download,
  History,
  Megaphone,
  MessageCircle,
} from "@multica/ui/icons";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";
import { useModalStore } from "@multica/core/modals";
import { useConfigStore } from "@multica/core/config";
import { instanceOrigin } from "@multica/core/config/instance-origin";
import { isDesktopShell } from "../platform/local-directory";
import { DISCORD_URL, DiscordIcon } from "./discord";
import { useT } from "../i18n";
import { docsUrl } from "../common/docs-url";
import { RAIL_ICON_BUTTON_CLASS } from "./rail-styles";
import { DOWNLOAD_URL } from "./help-links";
import { useWhatsNewMenuPanel, WhatsNewMenuPanel } from "../whats-new/whats-new-menu-panel";

const BRAND_URL = "https://multica.ai";

export function HelpLauncher() {
  const { t, i18n } = useT("layout");
  const serverVersion = useConfigStore((state) => state.serverVersion);
  const serverBuild = useConfigStore((state) => state.serverBuild);
  // Self-hosted instances serve their own /changelog; `daemon_app_url` is
  // omitted for the managed cloud, which keeps the brand site. Link to the
  // origin the visitor is on, not the canonical app URL.
  const appUrl = useConfigStore((state) => state.daemonAppUrl);
  const changelogUrl = `${(appUrl && instanceOrigin(appUrl)) || BRAND_URL}/changelog`;
  const displayVersion = serverVersion
    ? serverBuild
      ? `${serverVersion}-${serverBuild}`
      : serverVersion
    : "";
  // Web-only: offering "download the desktop app" inside the desktop app is
  // nonsense, and this rail is shared — apps/desktop renders the same app
  // shell as the web dashboard, so the entry has to be gated here.
  //
  // No `mounted` deferral (cf. browser-notification-setting.tsx): the desktop
  // renderer is a locally-bundled SPA with no SSR pass, and on web
  // `isDesktopShell()` is false both on the server and after hydration. The
  // markup matches either way, so the link can ship in the SSR payload instead
  // of popping in a frame late.
  const desktop = isDesktopShell();
  const triggerRef = useRef<HTMLButtonElement>(null);
  const whatsNew = useWhatsNewMenuPanel();
  return (
    <>
      <DropdownMenu>
        <TooltipHint label={t(($) => $.help.trigger)} side="right">
          <DropdownMenuTrigger
            ref={triggerRef}
            aria-label={t(($) => $.help.trigger)}
            className={RAIL_ICON_BUTTON_CLASS}
          >
            <CircleHelp />
          </DropdownMenuTrigger>
        </TooltipHint>
        <DropdownMenuContent
          align="end"
          side="right"
          sideOffset={8}
          className="min-w-40 max-w-56"
        >
          {!desktop && (
            <>
              <DropdownMenuItem
                render={
                  <a
                    href={DOWNLOAD_URL}
                    target="_blank"
                    rel="noopener noreferrer"
                  />
                }
              >
                <Download className="h-3.5 w-3.5" />
                {t(($) => $.help.download_desktop)}
                <ArrowUpRight className="size-3 translate-y-px text-faint-foreground" />
              </DropdownMenuItem>
              <DropdownMenuSeparator />
            </>
          )}
          <DropdownMenuItem
            render={
              <a
                href={docsUrl("", i18n.language)}
                target="_blank"
                rel="noopener noreferrer"
              />
            }
          >
            <BookOpen className="h-3.5 w-3.5" />
            {t(($) => $.help.docs)}
            <ArrowUpRight className="size-3 translate-y-px text-faint-foreground" />
          </DropdownMenuItem>
          {whatsNew.available && (
            <DropdownMenuItem onClick={whatsNew.show}>
              <Megaphone className="h-3.5 w-3.5" />
              {t(($) => $.whats_new.trigger)}
              {whatsNew.newestUnread && (
                <span
                  data-slot="whats-new-tag"
                  className="ml-auto text-caption font-medium text-brand tabular-nums"
                >
                  {`v${whatsNew.newestUnread}`}
                </span>
              )}
            </DropdownMenuItem>
          )}
          <DropdownMenuItem
            render={
              <a
                href={changelogUrl}
                target="_blank"
                rel="noopener noreferrer"
              />
            }
          >
            <History className="h-3.5 w-3.5" />
            {t(($) => $.help.changelog)}
            <ArrowUpRight className="size-3 translate-y-px text-faint-foreground" />
          </DropdownMenuItem>
          <DropdownMenuItem
            render={
              <a href={DISCORD_URL} target="_blank" rel="noopener noreferrer" />
            }
          >
            <DiscordIcon className="h-3.5 w-3.5" />
            {t(($) => $.help.discord)}
            <ArrowUpRight className="size-3 translate-y-px text-faint-foreground" />
          </DropdownMenuItem>
          <DropdownMenuItem
            onClick={() => useModalStore.getState().open("feedback")}
          >
            <MessageCircle className="h-3.5 w-3.5" />
            {t(($) => $.help.feedback)}
          </DropdownMenuItem>
          {displayVersion && (
            <>
              <DropdownMenuSeparator />
              {/* DropdownMenuLabel renders Base UI's Menu.GroupLabel, which reads
                  a Menu.Group context and throws if it has no Group ancestor. It
                  must always be wrapped in a DropdownMenuGroup — without it the
                  Help menu crashes the whole app on open (no error boundary sits
                  above the rail). */}
              <DropdownMenuGroup>
                <DropdownMenuLabel className="font-normal break-words">
                  {t(($) => $.help.server_version, { version: displayVersion })}
                </DropdownMenuLabel>
              </DropdownMenuGroup>
            </>
          )}
        </DropdownMenuContent>
      </DropdownMenu>
      <WhatsNewMenuPanel panel={whatsNew} anchor={triggerRef} />
    </>
  );
}
