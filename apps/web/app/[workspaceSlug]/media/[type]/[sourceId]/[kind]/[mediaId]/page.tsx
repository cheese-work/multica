"use client";

import { use } from "react";
import { usePathname } from "next/navigation";
import { parseMediaPath } from "@multica/core/paths";
import { MediaPage, MediaUnavailablePage } from "@multica/views/media";

export default function MediaWebPage({ params }: { params: Promise<{ workspaceSlug: string; type: string; sourceId: string; kind: string; mediaId: string }> }) {
  use(params);
  const pathname = usePathname();
  const route = parseMediaPath(pathname);
  return <div className="h-svh w-full">{route ? <MediaPage key={pathname} route={route} /> : <MediaUnavailablePage />}</div>;
}
