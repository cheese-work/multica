"use client";

import { lazy, Suspense, useState } from "react";
import { useSearchStore } from "@multica/core/command-bar";

const SearchCommandImpl = lazy(() =>
  import("./search-command").then((m) => ({ default: m.SearchCommand })),
);

/**
 * The palette (cmdk plus every page and delegate view) loads the first time it
 * opens, then stays mounted so close animations and state behave as before.
 */
export function SearchCommand({ initialQuery }: { initialQuery?: string } = {}) {
  const open = useSearchStore((s) => s.open);
  const [wasOpened, setWasOpened] = useState(open);
  if (open && !wasOpened) setWasOpened(true);
  if (!wasOpened) return null;
  return (
    <Suspense fallback={null}>
      <SearchCommandImpl initialQuery={initialQuery} />
    </Suspense>
  );
}
