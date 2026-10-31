"use client";

import { useCallback, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { clearClientSessionData } from "@multica/core/platform";
import { paths } from "@multica/core/paths";
import { unsubscribeThisDevice } from "@multica/core/push";
import { useT } from "../i18n";
import { useNavigation } from "../navigation";
import { isDesktopShell } from "../platform";

/**
 * Performs a complete logout: ends the server session, then clears per-workspace
 * client storage, legacy cookies, the desktop tab state, the entire React Query
 * cache, the in-memory auth store, and finally navigates to /login. Wraps what
 * was previously duplicated in app-sidebar's logout handler so NoAccessPage's
 * "Sign in as a different user" and any future entry point can use the same
 * flow.
 *
 * With cookie auth the server holds the session (and, with it, the push
 * subscriptions registered by it), so nothing on the client is cleaned up until
 * the server confirms. If it does not, the user stays signed in with a visible
 * error and can press the button again; a retry that succeeds cleans up exactly
 * once. Token mode (desktop, legacy web) has no server session and logs out at
 * once, and a session the server already ended (401) never comes through here.
 *
 * Without a unified logout, callers that only do `navigate('/login')`
 * leave the auth cookie + React Query cache + local storage intact —
 * AuthInitializer then silently re-authenticates the user on the login
 * page and redirects them back where they came from.
 *
 * The returned function resolves to whether the logout completed. It never
 * rejects: a failure is shown here, so callers can hand it straight to an
 * onClick without leaving a promise unhandled.
 */
export function useLogout() {
  const queryClient = useQueryClient();
  const endServerSession = useAuthStore((s) => s.endServerSession);
  const authLogout = useAuthStore((s) => s.logout);
  const { push } = useNavigation();
  const { t } = useT("auth");
  const inFlight = useRef<Promise<boolean> | null>(null);

  return useCallback((): Promise<boolean> => {
    inFlight.current ??= (async () => {
      // Web only, and before the session ends: the server delete needs it.
      // Best effort, so a push failure never keeps the user signed in.
      if (!isDesktopShell()) {
        await unsubscribeThisDevice(api).catch(() => {});
      }
      try {
        await endServerSession();
      } catch {
        toast.error(t(($) => $.errors.logout_failed));
        return false;
      }
      // Shared with the session-expiry path, which has to erase exactly the
      // same client-side state — see core's platform/session-cleanup for what
      // and why.
      clearClientSessionData(queryClient);
      authLogout();

      // Navigate to /login explicitly. authLogout() clears state but doesn't
      // move the URL — without this the caller might be on a workspace URL
      // which renders null (layout gates on user) and leaves the user
      // stuck on a blank page.
      push(paths.login());
      return true;
    })().finally(() => {
      inFlight.current = null;
    });
    return inFlight.current;
  }, [queryClient, endServerSession, authLogout, push, t]);
}
