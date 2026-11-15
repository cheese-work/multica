"use client";

import { lazy, Suspense, type ComponentProps } from "react";

const IssueDetailImpl = lazy(() =>
  import("./issue-detail").then((m) => ({ default: m.IssueDetail })),
);

/**
 * IssueDetail with its editor, comment and timeline code split out. Hosts that
 * render it only after a selection (inbox, peek) use this so the list route
 * does not ship the detail surface.
 */
export function IssueDetail(props: ComponentProps<typeof IssueDetailImpl>) {
  return (
    <Suspense fallback={null}>
      <IssueDetailImpl {...props} />
    </Suspense>
  );
}
