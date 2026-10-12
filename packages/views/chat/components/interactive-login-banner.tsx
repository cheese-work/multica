"use client";

import { useT } from "../../i18n";

export function InteractiveLoginBanner() {
  const { t } = useT("chat");
  return (
    <div role="alert" className="p-4 text-body-sm text-muted-foreground">
      {t(($) => $.page.interactive_login_required)}
    </div>
  );
}
