"use client";

import { useEffect, useRef } from "react";
import { FOCUS_WAKEUPS } from "@multica/core/paths";
import { useOptionalNavigation } from "../../navigation";

/**
 * Arriving by `?focus=wakeups` (a project or issue page's wakeup-rules deep
 * link): call `onFocus` once to reveal the rules, then clear the param so a
 * refresh does not replay it. A second click must land again, and so must one
 * aimed at a different resource on the same route.
 *
 * While `enabled` is false the param stays in the URL: a page that is still
 * loading has no section to reveal yet, and consuming the link then loses it.
 *
 * Navigation is read optionally: the sections that host this are leaves that
 * tests mount in isolation, and without an adapter there is nothing to focus.
 */
export function useWakeupsFocus(resourceId: string, onFocus: () => void, enabled = true) {
  const navigation = useOptionalNavigation();
  const handledForRef = useRef<string | null>(null);
  // The latest callback, so the effect below does not re-run on every render of an unstable one.
  const onFocusRef = useRef(onFocus);
  onFocusRef.current = onFocus;

  useEffect(() => {
    if (!navigation || !enabled) return;
    if (navigation.searchParams.get("focus") !== FOCUS_WAKEUPS) {
      // Ordinary visit, or the `replace` below has landed: re-arm.
      handledForRef.current = null;
      return;
    }
    // The adapter object is not referentially stable, so this effect re-runs
    // on renders the `replace` itself causes. Keyed by resource so a link
    // aimed at a different one is never swallowed.
    if (handledForRef.current === resourceId) return;
    handledForRef.current = resourceId;
    onFocusRef.current();
    const params = new URLSearchParams(navigation.searchParams);
    params.delete("focus");
    const query = params.toString();
    navigation.replace(query ? `${navigation.pathname}?${query}` : navigation.pathname);
  }, [resourceId, navigation, enabled]);
}
