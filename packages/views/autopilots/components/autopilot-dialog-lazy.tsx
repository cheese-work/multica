"use client";

import { lazy, Suspense, type ComponentProps } from "react";

// The form pulls in the schedule editor and the rich-text editor, and it only
// mounts when a user opens it. Every importer goes through this wrapper so no
// route carries the dialog before then.
const Dialog = lazy(() =>
  import("./autopilot-dialog").then((m) => ({ default: m.AutopilotDialog })),
);

export function AutopilotDialog(props: ComponentProps<typeof Dialog>) {
  return (
    <Suspense fallback={null}>
      <Dialog {...props} />
    </Suspense>
  );
}
