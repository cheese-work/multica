"use client";

import { useCallback, useMemo } from "react";
import { Input } from "@multica/ui/components/ui/input";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { toast } from "sonner";
import { useAuthStore } from "@multica/core/auth";
import { api } from "@multica/core/api";
import { useAccountProfileDraftStore } from "@multica/core/settings/draft-stores";
import { AvatarUploadControl } from "../../common/avatar-upload-control";
import { useT } from "../../i18n";
import {
  SettingsCard,
  SettingsRow,
  SettingsSaveState,
  SettingsSection,
  SettingsTab,
} from "./settings-layout";
import { useAutoSave } from "./use-auto-save";
import { useSettingsDraft } from "./use-settings-draft";

// Mirror server/internal/handler/auth.go:MaxProfileDescriptionLen. Counted in
// JS String.length (UTF-16 code units) here while the server counts runes,
// so a profile full of supplementary-plane emoji will trip the client cap
// before the server's — which is the safer direction of drift.
const MAX_PROFILE_DESCRIPTION_LEN = 2000;

interface ProfileDraft {
  name: string;
  profileDescription: string;
}

function profilesEqual(left: ProfileDraft, right: ProfileDraft) {
  return left.name === right.name && left.profileDescription === right.profileDescription;
}

export function AccountTab() {
  const { t } = useT("settings");
  const user = useAuthStore((s) => s.user);
  const setUser = useAuthStore((s) => s.setUser);

  const savedFields = useMemo(
    () => ({ name: user?.name ?? "", profileDescription: user?.profile_description ?? "" }),
    [user?.name, user?.profile_description],
  );
  // Keyed on user id and persisted: edits survive remounts and reloads, and an
  // avatar upload or auto-save replacing the user object never wipes them.
  const [fields, setFields, confirmFields] = useSettingsDraft(
    useAccountProfileDraftStore,
    user?.id ?? "",
    savedFields,
  );
  const profileName = fields.name;
  const profileDescription = fields.profileDescription;
  const setProfileName = (name: string) => setFields({ name });
  const setProfileDescription = (profileDescription: string) => setFields({ profileDescription });

  const descriptionTooLong = profileDescription.length > MAX_PROFILE_DESCRIPTION_LEN;

  const draft = useMemo(
    () => ({ name: profileName, profileDescription }),
    [profileDescription, profileName],
  );
  const saveProfile = useCallback(
    async (next: ProfileDraft) => {
      const updated = await api.updateMe({
        name: next.name,
        profile_description: next.profileDescription,
      });
      setUser(updated);
    },
    [setUser],
  );
  const autoSave = useAutoSave({
    value: draft,
    savedValue: savedFields,
    onSave: saveProfile,
    onSuccess: confirmFields,
    onIdle: confirmFields,
    onError: (error) =>
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.account.toast_profile_failed),
      ),
    enabled: !!user && !!profileName.trim() && !descriptionTooLong,
    isEqual: profilesEqual,
  });

  return (
    <SettingsTab title={t(($) => $.page.tabs.profile)} scope="account">
      <SettingsSection
        title={t(($) => $.account.section_profile)}
        action={
          <SettingsSaveState
            status={autoSave.status}
            savingLabel={t(($) => $.auto_save.saving)}
            savedLabel={t(($) => $.auto_save.saved)}
            errorLabel={t(($) => $.auto_save.failed)}
          />
        }
      >
        <SettingsCard>
          <SettingsRow
            anchor="avatar"
            label={t(($) => $.account.avatar_label)}
            description={t(($) => $.account.click_avatar_hint)}
            size="none"
          >
            <div className="flex justify-start sm:justify-end">
              <AvatarUploadControl
                variant="user"
                value={user?.avatar_url ?? null}
                name={user?.name ?? ""}
                size={64}
                onUploaded={async (url) => {
                  try {
                    const updated = await api.updateMe({ avatar_url: url });
                    setUser(updated);
                  } catch (error) {
                    toast.error(
                      error instanceof Error
                        ? error.message
                        : t(($) => $.account.toast_avatar_failed),
                    );
                  }
                }}
              />
            </div>
          </SettingsRow>

          <SettingsRow
            anchor="name"
            label={t(($) => $.account.name_label)}
            size="text"
          >
            <Input
              type="text"
              name="profile-name"
              autoComplete="name"
              aria-label={t(($) => $.account.name_label)}
              value={profileName}
              onChange={(event) => setProfileName(event.target.value)}
              onBlur={autoSave.flush}
            />
          </SettingsRow>

          <SettingsRow
            anchor="about"
            label={t(($) => $.account.profile_description_label)}
            description={t(($) => $.account.profile_description_hint)}
            size="text"
            align="start"
          >
            <div>
              <Textarea
                name="profile-description"
                autoComplete="off"
                aria-label={t(($) => $.account.profile_description_label)}
                value={profileDescription}
                onChange={(event) => setProfileDescription(event.target.value)}
                onBlur={autoSave.flush}
                placeholder={t(($) => $.account.profile_description_placeholder)}
                rows={5}
                maxLength={MAX_PROFILE_DESCRIPTION_LEN}
                aria-invalid={descriptionTooLong}
                className="resize-y"
              />
              <div className="mt-1 flex justify-end text-caption text-muted-foreground">
                <span
                  className={descriptionTooLong ? "text-destructive shrink-0" : "shrink-0"}
                  aria-live="polite"
                >
                  {profileDescription.length}/{MAX_PROFILE_DESCRIPTION_LEN}
                </span>
              </div>
              {descriptionTooLong ? (
                <p className="mt-1 text-caption text-destructive">
                  {t(($) => $.account.profile_description_too_long, {
                    max: MAX_PROFILE_DESCRIPTION_LEN,
                    count: profileDescription.length,
                  })}
                </p>
              ) : null}
            </div>
          </SettingsRow>
        </SettingsCard>
      </SettingsSection>
    </SettingsTab>
  );
}
