import { createKeyedDraftStore } from "../drafts/create-keyed-draft-store";

/**
 * Unsent settings input, kept per entity key so it outlives a remount, a route
 * change or a reload. Each field is optional: an unset field follows the saved
 * value, so only what the user changed (and the server has not yet confirmed)
 * is stored.
 */
export interface SettingsFieldsDraft {
  [field: string]: string | undefined;
}
const anyField = (d: SettingsFieldsDraft) => Object.values(d).some((v) => v !== undefined);

/** Key: workspace id. Fields: name, description, context, compactPrompt. */
export const useWorkspaceSettingsDraftStore = createKeyedDraftStore<SettingsFieldsDraft>({
  storageKey: "multica_workspace_settings_drafts",
  emptyData: {},
  hasMeaningful: anyField,
});

/** Key: user id. Fields: name, profileDescription. Not workspace-scoped. */
export const useAccountProfileDraftStore = createKeyedDraftStore<SettingsFieldsDraft>({
  storageKey: "multica_account_profile_drafts",
  emptyData: {},
  hasMeaningful: anyField,
  workspaceScoped: false,
});

/** Key: workspace id. Fields: email, role. */
export const useInviteDraftStore = createKeyedDraftStore<SettingsFieldsDraft>({
  storageKey: "multica_invite_drafts",
  emptyData: {},
  hasMeaningful: anyField,
});

/** Key: `<scope kind>:<scope id>:<rule key or "new">`. Fields: name, form (JSON of the rule draft). */
export const useWakeupRuleDraftStore = createKeyedDraftStore<SettingsFieldsDraft>({
  storageKey: "multica_wakeup_rule_drafts",
  emptyData: {},
  hasMeaningful: anyField,
});

/**
 * Key: `workspace:<plugin name>`. Fields: action, message, and `base`, the JSON of the
 * policy version (policy + sha256) the edit started from, frozen so the passkey
 * signs against it and a concurrent change surfaces as a 409, never an overwrite.
 */
export const useGuardEditDraftStore = createKeyedDraftStore<SettingsFieldsDraft>({
  storageKey: "multica_guard_edit_drafts",
  emptyData: {},
  hasMeaningful: anyField,
});
