"use client";

import { lazy, Suspense, type ComponentProps } from "react";
import type { AutopilotDialog as AutopilotDialogForm } from "./autopilot-dialog";

// The form carries the rich-text editor. Every caller goes through this wrapper,
// so the editor loads when a dialog opens and the dynamic import stays effective.
const Form = lazy(() =>
  import("./autopilot-dialog").then((m) => ({ default: m.AutopilotDialog })),
);

export function AutopilotDialog(props: ComponentProps<typeof AutopilotDialogForm>) {
  return (
    <Suspense fallback={null}>
      <Form {...props} />
    </Suspense>
  );
}
