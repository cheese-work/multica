"use client";

import { Button } from "@multica/ui/components/ui/button";
import { useLogout } from "../../auth/use-logout";
import { useT } from "../../i18n";

export function InteractiveLoginBanner() {
  const { t } = useT("chat");
  const signIn = useLogout();
  return (
    <div
      role="alert"
      className="flex flex-wrap items-center gap-3 p-4 text-body-sm text-muted-foreground"
    >
      <span>{t(($) => $.page.interactive_login_required)}</span>
      <Button type="button" variant="outline" size="sm" onClick={signIn}>
        {t(($) => $.page.interactive_login_action)}
      </Button>
    </div>
  );
}
