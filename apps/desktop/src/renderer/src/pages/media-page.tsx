import { useLocation } from "react-router-dom";
import { parseMediaPath } from "@multica/core/paths";
import { MediaPage, MediaUnavailablePage } from "@multica/views/media";

export function MediaPageRoute() {
  const { pathname } = useLocation();
  const route = parseMediaPath(pathname);
  return route ? <MediaPage key={pathname} route={route} /> : <MediaUnavailablePage />;
}
