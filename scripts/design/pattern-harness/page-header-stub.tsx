import type { ReactNode } from "react";
import { cn } from "@multica/ui/lib/utils";

// Same header markup as packages/views/layout/page-header.tsx. The real file pulls app-shell-context ->
// navigation, which hangs headless Chrome at module init. create-shell always passes `leading`, so the
// sidebar trigger fallback is never rendered.
export function PageHeader({ children, leading, className }: { children: ReactNode; leading?: ReactNode; className?: string }) {
  return <header className={cn("flex h-12 shrink-0 items-center gap-2 border-b", className, "px-4")}>{leading}{children}</header>;
}
