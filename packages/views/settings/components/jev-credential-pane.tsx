"use client";

import { useState } from "react";
import { type UseQueryResult } from "@tanstack/react-query";
import { Check, Minus } from "lucide-react";
import { ApiError } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { useRemoveJevCredential, useSetJevCredential } from "@multica/core/workspace/mutations";
import type { JevCredentialPresence, WorkspaceJevGovernance } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { useT } from "../../i18n";
import { SettingsCard, SettingsRow, SettingsSection } from "./settings-layout";
import { JevApiUrlRow } from "./jev-api-url-row";
import { problemOf } from "./jev-problem";

const NOT_CONFIGURED = "jev_credential_configuration_required";

/**
 * The Jev provider key. The page only ever learns whether a key is stored: the
 * server has no endpoint that returns it, and nothing here keeps what an admin
 * types once the request is done (the field is cleared, the mutations drop
 * their variables, and no query cache holds it).
 */
export function JevCredentialPane({
  credential,
  config,
  canManage,
  online,
  onNotice,
}: {
  credential: UseQueryResult<JevCredentialPresence>;
  config: WorkspaceJevGovernance;
  canManage: boolean;
  online: boolean;
  onNotice: (notice: "key_saved" | "key_removed" | "url_saved" | "url_cleared") => void;
}) {
  const { t } = useT("settings");
  const [keyDialog, setKeyDialog] = useState(false);
  const [removeDialog, setRemoveDialog] = useState(false);
  // Set once a save is refused with 403; the load-time capability covers the rest.
  const [refused, setRefused] = useState(false);

  if (credential.isError && !credential.data) {
    return (
      <div
        role="alert"
        className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-destructive/30 bg-destructive/5 px-4 py-3"
      >
        <p className="text-body font-medium">{t(($) => $.jev_governance.credential.load_error)}</p>
        <Button variant="outline" size="sm" onClick={() => void credential.refetch()}>
          {t(($) => $.jev_governance.retry)}
        </Button>
      </div>
    );
  }
  if (!credential.data) {
    return (
      <div role="status" aria-label={t(($) => $.jev_governance.credential.loading)} className="space-y-3">
        <Skeleton className="h-16 w-full rounded-xl" />
      </div>
    );
  }

  const present = credential.data.present;
  // The server said it cannot store keys at all: stop offering to.
  const unconfigured = refused || credential.data.configurable === false;
  const editable = canManage && online;
  return (
    <SettingsSection anchor="credential">
      <SettingsCard>
        <SettingsRow
          label={
            <span className="flex items-center gap-2">
              {t(($) => $.jev_governance.credential.title)}
              <span className="inline-flex items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-caption font-normal text-muted-foreground">
                {present ? (
                  <Check aria-hidden="true" className="size-3 text-success" />
                ) : (
                  <Minus aria-hidden="true" className="size-3" />
                )}
                {present ? t(($) => $.jev_governance.credential.set) : t(($) => $.jev_governance.credential.not_set)}
              </span>
            </span>
          }
          description={t(($) => $.jev_governance.credential.hint)}
          size="none"
        >
          {canManage ? (
            <div data-testid="jev-credential-actions" className="flex flex-wrap items-center gap-2">
              <Button
                variant={present ? "outline" : "default"}
                size="sm"
                disabled={!editable || unconfigured}
                onClick={() => setKeyDialog(true)}
              >
                {present ? t(($) => $.jev_governance.credential.replace) : t(($) => $.jev_governance.credential.set_key)}
              </Button>
              {present ? (
                <Button
                  variant="ghost"
                  size="sm"
                  className="text-destructive hover:text-destructive"
                  disabled={!editable}
                  onClick={() => setRemoveDialog(true)}
                >
                  {t(($) => $.jev_governance.credential.remove)}
                </Button>
              ) : null}
            </div>
          ) : null}
        </SettingsRow>
        <JevApiUrlRow config={config} canManage={canManage} online={online} onNotice={onNotice} />
      </SettingsCard>
      {unconfigured ? (
        <p data-testid="jev-credential-unconfigured" role="note" className="text-caption text-muted-foreground">
          {t(($) => $.jev_governance.credential.unconfigured)}
        </p>
      ) : null}

      <KeyDialog
        open={keyDialog}
        replacing={present}
        online={online}
        onClose={() => setKeyDialog(false)}
        onSaved={() => {
          setKeyDialog(false);
          onNotice("key_saved");
        }}
        onUnconfigured={() => {
          setKeyDialog(false);
          setRefused(true);
        }}
      />
      <RemoveDialog
        open={removeDialog}
        online={online}
        onClose={() => setRemoveDialog(false)}
        onRemoved={() => {
          setRemoveDialog(false);
          onNotice("key_removed");
        }}
      />
    </SettingsSection>
  );
}

function KeyDialog({
  open,
  replacing,
  online,
  onClose,
  onSaved,
  onUnconfigured,
}: {
  open: boolean;
  replacing: boolean;
  online: boolean;
  onClose: () => void;
  onSaved: () => void;
  onUnconfigured: () => void;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const save = useSetJevCredential(wsId);
  // Held in component state only, and cleared on every way out of the dialog.
  const [value, setValue] = useState("");
  const [failed, setFailed] = useState(false);
  const trimmed = value.trim();

  const finish = () => {
    setValue("");
    setFailed(false);
    save.reset();
  };
  const close = () => {
    if (save.isPending) return;
    finish();
    onClose();
  };
  const submit = async () => {
    if (!trimmed || !online || save.isPending) return;
    setFailed(false);
    try {
      await save.mutateAsync(trimmed);
      finish();
      onSaved();
    } catch (error) {
      if (error instanceof ApiError && error.status === 403 && problemOf(error) === NOT_CONFIGURED) {
        finish();
        onUnconfigured();
        return;
      }
      // The typed key stays in `value` for a retry; drop the mutation's copy.
      save.reset();
      setFailed(true);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(next) => (next ? undefined : close())}>
      <DialogContent data-testid="jev-key-dialog">
        <form
          onSubmit={(event) => {
            event.preventDefault();
            void submit();
          }}
          className="grid gap-4"
        >
          <DialogHeader>
            <DialogTitle>
              {replacing
                ? t(($) => $.jev_governance.credential.replace_title)
                : t(($) => $.jev_governance.credential.set_title)}
            </DialogTitle>
          </DialogHeader>
          <div className="grid gap-1.5">
            <label htmlFor="jev-api-key" className="text-body font-medium">
              {t(($) => $.jev_governance.credential.api_key)}
            </label>
            <Input
              id="jev-api-key"
              name="jev-api-key"
              type="password"
              autoComplete="off"
              spellCheck={false}
              value={value}
              disabled={save.isPending}
              onChange={(event) => {
                setFailed(false);
                setValue(event.target.value);
              }}
            />
            <DialogDescription className="text-caption">
              {replacing
                ? t(($) => $.jev_governance.credential.replace_hint)
                : t(($) => $.jev_governance.credential.set_hint)}
            </DialogDescription>
          </div>
          {failed ? (
            <p role="alert" className="text-caption text-destructive">
              {t(($) => $.jev_governance.credential.save_failed)}
            </p>
          ) : null}
          <DialogFooter>
            <DialogClose render={<Button type="button" variant="outline" disabled={save.isPending} />}>
              {t(($) => $.jev_governance.cancel)}
            </DialogClose>
            <Button
              type="submit"
              disabled={!trimmed || !online || save.isPending}
              aria-busy={save.isPending ? true : undefined}
            >
              {t(($) => $.jev_governance.credential.save_key)}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function RemoveDialog({
  open,
  online,
  onClose,
  onRemoved,
}: {
  open: boolean;
  online: boolean;
  onClose: () => void;
  onRemoved: () => void;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const remove = useRemoveJevCredential(wsId);
  const [failed, setFailed] = useState(false);

  const close = () => {
    if (remove.isPending) return;
    setFailed(false);
    remove.reset();
    onClose();
  };
  const confirm = async () => {
    setFailed(false);
    try {
      await remove.mutateAsync();
      remove.reset();
      onRemoved();
    } catch {
      setFailed(true);
    }
  };

  return (
    <AlertDialog open={open} onOpenChange={(next) => (next ? undefined : close())}>
      <AlertDialogContent data-testid="jev-remove-key-dialog">
        <AlertDialogHeader>
          <AlertDialogTitle>{t(($) => $.jev_governance.credential.remove_title)}</AlertDialogTitle>
          <AlertDialogDescription>{t(($) => $.jev_governance.credential.remove_body)}</AlertDialogDescription>
        </AlertDialogHeader>
        {failed ? (
          <p role="alert" className="text-caption text-destructive">
            {t(($) => $.jev_governance.credential.remove_failed)}
          </p>
        ) : null}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={remove.isPending}>{t(($) => $.jev_governance.cancel)}</AlertDialogCancel>
          <Button
            variant="destructive"
            disabled={!online || remove.isPending}
            aria-busy={remove.isPending ? true : undefined}
            onClick={() => void confirm()}
          >
            {t(($) => $.jev_governance.credential.remove_confirm)}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
