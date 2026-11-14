// Harness-only stand-ins (CHE-5532). The settings-navigation view renders the real SettingsPage shell, so every
// tab panel and the three core hooks it reads are replaced here (same split as settings-page.test.tsx).
// Profile keeps the real SettingsTab/Section/Row so the content column shows the settings-page pattern.
import { SettingsCard, SettingsRow, SettingsSection, SettingsTab } from "../../../packages/views/settings/components/settings-layout";

export function AccountTab() {
  return (
    <SettingsTab title="Profile" description="How you appear to your team." scope="account">
      <SettingsSection title="Identity">
        <SettingsCard>
          <SettingsRow label="Name" description="Shown on comments and mentions." size="text">
            <span className="text-body">Ada Lovelace</span>
          </SettingsRow>
        </SettingsCard>
      </SettingsSection>
    </SettingsTab>
  );
}
const Empty = () => <div className="text-body text-muted-foreground">Tab content is outside this pattern.</div>;
export const PreferencesTab = Empty, TokensTab = Empty, PasskeysTab = Empty, WorkspaceTab = Empty, MembersTab = Empty,
  CodeTab = Empty, ChannelsTab = Empty, ConnectedAppsTab = Empty, NotificationsTab = Empty, LabelsTab = Empty,
  IssueStatusesTab = Empty, WorkflowsTab = Empty, PropertiesTab = Empty, QuickActionsTab = Empty,
  KeyboardShortcutsTab = Empty, PluginsTab = Empty, McpTab = Empty, BillingTab = Empty, ExportPrivacyTab = Empty,
  WakeupsTab = Empty, ProviderBreakersTab = Empty;
export const useComposioAvailable = () => false;
