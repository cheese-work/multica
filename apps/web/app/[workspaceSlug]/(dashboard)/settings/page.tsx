"use client";

import { SettingsPage } from "@multica/views/settings";
import { useT } from "@multica/views/i18n";
import { Search } from "@multica/ui/icons";
import { SiteSearchTab } from "@/components/site-search-tab";

export default function Page() {
  const { t } = useT("settings");
  return (
    <SettingsPage extraDeviceTabs={[{
      value: "site-search",
      label: t(($) => $.site_search.title),
      icon: Search,
      content: <SiteSearchTab />,
    }]} />
  );
}
