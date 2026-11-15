"use client";

import { useCallback, useEffect, useState } from "react";
import { Trash2 } from "@multica/ui/icons";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { createPasskey, signPasskeyChallenge } from "@multica/core/auth/webauthn";
import type { PasskeySummary } from "@multica/core/api/passkey-schema";
import { Button } from "@multica/ui/components/ui/button";
import { Badge } from "@multica/ui/components/ui/badge";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Input } from "@multica/ui/components/ui/input";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useLocale, useT } from "../../i18n";
import { useNavigation } from "../../navigation";
import { openExternal } from "../../platform";
import { isDesktopShell } from "../../platform/local-directory";
import { SettingsSection, SettingsTab } from "./settings-layout";
import { settingsHref } from "./settings-navigation";

const errorMessage = (error: unknown, fallback: string) => (error instanceof Error ? error.message : fallback);

export function PasskeysTab() {
  const { t } = useT("settings");
  const navigation = useNavigation();
  const desktop = isDesktopShell();

  if (desktop) {
    // Passkeys bind to the web origin, so the desktop shell hands off to the web page.
    const webUrl = navigation.getShareableUrl(settingsHref(navigation.pathname, new URLSearchParams(), "passkeys"));
    return (
      <SettingsTab title={t(($) => $.passkeys.title)} description={t(($) => $.passkeys.purpose)} scope="account">
        <SettingsSection description={t(($) => $.passkeys.desktop_note)}>
          <div>
            <Button onClick={() => openExternal(webUrl)}>{t(($) => $.passkeys.open_in_browser)}</Button>
          </div>
        </SettingsSection>
      </SettingsTab>
    );
  }
  return <PasskeysManager />;
}

function PasskeysManager() {
  const { t } = useT("settings");
  const locale = useLocale();
  const [passkeys, setPasskeys] = useState<PasskeySummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadFailed, setLoadFailed] = useState(false);
  const [ceremony, setCeremony] = useState<Awaited<ReturnType<typeof api.startPasskeyEnrollment>> | null>(null);
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [revokeId, setRevokeId] = useState<string | null>(null);
  const hasOtherSigner = passkeys.some((p) => p.id !== revokeId && p.state === "active");
  // Sole-key revoke: no other active key can sign, so a mailed code stands in for the assertion.
  const [codeRevoke, setCodeRevoke] = useState<{ id: string; ceremonyId: string; tag?: string } | null>(null);

  const load = useCallback(async () => {
    try {
      setPasskeys(await api.listPasskeys());
      setLoadFailed(false);
    } catch {
      setLoadFailed(true);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const startEnrollment = async () => {
    setBusy(true);
    try {
      setCeremony(await api.startPasskeyEnrollment());
      setCode("");
    } catch (e) {
      toast.error(errorMessage(e, t(($) => $.passkeys.toast_enroll_failed)));
    } finally {
      setBusy(false);
    }
  };

  const finishEnrollment = async () => {
    if (!ceremony) return;
    setBusy(true);
    try {
      // The gate assertion comes first: it expires on its own TTL, and the code step is the slow one.
      const assertion = ceremony.assertion ? await signPasskeyChallenge(ceremony.assertion) : undefined;
      const attestation = await createPasskey(ceremony.options);
      await api.finishPasskeyEnrollment({
        ceremony_id: ceremony.ceremony_id, code: code.trim(), attestation, ...(assertion && { assertion }),
      });
      setCeremony(null);
      toast.success(t(($) => $.passkeys.toast_enrolled));
      await load();
    } catch (e) {
      toast.error(errorMessage(e, t(($) => $.passkeys.toast_enroll_failed)));
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (id: string, forceCode = false) => {
    setBusy(true);
    try {
      // The mailed code also covers a signer that exists but is lost; the server still enforces the gate floor.
      if (forceCode || !passkeys.some((p) => p.id !== id && p.state === "active")) {
        const started = await api.startPasskeyRevocationByCode(id);
        setCodeRevoke({ id, ceremonyId: started.ceremony_id, tag: started.tag });
        setCode("");
        return;
      }
      const challenge = await api.challengePasskeyRevocation(id);
      await api.revokePasskey(id, await signPasskeyChallenge(challenge));
      toast.success(t(($) => $.passkeys.toast_revoked));
      await load();
    } catch (e) {
      toast.error(errorMessage(e, t(($) => $.passkeys.toast_revoke_failed)));
    } finally {
      setBusy(false);
    }
  };

  const finishCodeRevoke = async () => {
    if (!codeRevoke) return;
    setBusy(true);
    try {
      await api.confirmPasskeyRevocationByCode(codeRevoke.id, { ceremony_id: codeRevoke.ceremonyId, code: code.trim() });
      setCodeRevoke(null);
      toast.success(t(($) => $.passkeys.toast_revoked));
      await load();
    } catch (e) {
      toast.error(errorMessage(e, t(($) => $.passkeys.toast_revoke_failed)));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SettingsTab title={t(($) => $.passkeys.title)} description={t(($) => $.passkeys.purpose)} scope="account">
      <SettingsSection description={t(($) => $.passkeys.description)}>
        {codeRevoke ? (
          <Card>
            <CardContent className="space-y-3">
              <p className="text-caption text-muted-foreground">{t(($) => $.passkeys.revoke_code_hint)}</p>
              {codeRevoke.tag && <p className="text-caption text-muted-foreground">{t(($) => $.passkeys.attempt_tag, { tag: codeRevoke.tag })}</p>}
              <div className="grid gap-3 sm:grid-cols-[1fr_auto_auto]">
                <Input
                  autoComplete="one-time-code"
                  inputMode="numeric"
                  aria-label={t(($) => $.passkeys.code_label)}
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                />
                <Button onClick={finishCodeRevoke} disabled={busy || !code.trim()}>
                  {t(($) => $.passkeys.confirm)}
                </Button>
                <Button variant="ghost" onClick={() => setCodeRevoke(null)} disabled={busy}>
                  {t(($) => $.passkeys.cancel)}
                </Button>
              </div>
            </CardContent>
          </Card>
        ) : ceremony ? (
          <Card>
            <CardContent className="space-y-3">
              <p className="text-caption text-muted-foreground">{t(($) => $.passkeys.code_hint)}</p>
              {ceremony.tag && <p className="text-caption text-muted-foreground">{t(($) => $.passkeys.attempt_tag, { tag: ceremony.tag })}</p>}
              <div className="grid gap-3 sm:grid-cols-[1fr_auto_auto]">
                <Input
                  autoComplete="one-time-code"
                  inputMode="numeric"
                  aria-label={t(($) => $.passkeys.code_label)}
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                />
                <Button onClick={finishEnrollment} disabled={busy || !code.trim()}>
                  {t(($) => $.passkeys.confirm)}
                </Button>
                <Button variant="ghost" onClick={() => setCeremony(null)} disabled={busy}>
                  {t(($) => $.passkeys.cancel)}
                </Button>
              </div>
            </CardContent>
          </Card>
        ) : (
          <div>
            <Button onClick={startEnrollment} disabled={busy}>{t(($) => $.passkeys.add)}</Button>
          </div>
        )}

        {loading ? null : loadFailed ? (
          <p className="text-caption text-muted-foreground">{t(($) => $.passkeys.load_failed)}</p>
        ) : passkeys.length === 0 ? (
          <p className="text-caption text-muted-foreground">{t(($) => $.passkeys.empty)}</p>
        ) : (
          <div className="space-y-2">
            {passkeys.map((passkey, index) => (
              <Card key={passkey.id}>
                <CardContent className="flex items-center gap-3">
                  <div className="min-w-0 flex-1">
                    <div className="text-body font-medium truncate">
                      {t(($) => $.passkeys.item_title, { number: index + 1 })}
                      {passkey.state === "suspect" && (
                        <Badge variant="destructive" className="ml-2">{t(($) => $.passkeys.suspect)}</Badge>
                      )}
                    </div>
                    <div className="text-caption text-muted-foreground">
                      {t(($) => $.passkeys.item_meta, {
                        created: new Date(passkey.created_at).toLocaleDateString(locale),
                        lastUsed: passkey.last_used_at
                          ? t(($) => $.passkeys.last_used_with_date, {
                              date: new Date(passkey.last_used_at!).toLocaleDateString(locale),
                            })
                          : t(($) => $.passkeys.last_used_never),
                      })}
                    </div>
                  </div>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    disabled={busy}
                    onClick={() => setRevokeId(passkey.id)}
                    aria-label={t(($) => $.passkeys.revoke_aria, { number: index + 1 })}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </Button>
                </CardContent>
              </Card>
            ))}
          </div>
        )}
      </SettingsSection>

      <AlertDialog open={!!revokeId} onOpenChange={(v) => { if (!v) setRevokeId(null); }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.passkeys.revoke_dialog.title)}</AlertDialogTitle>
            <AlertDialogDescription>{t(($) => $.passkeys.revoke_dialog.description)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.passkeys.revoke_dialog.cancel)}</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              onClick={async () => {
                const id = revokeId;
                setRevokeId(null);
                if (id) await revoke(id);
              }}
            >
              {t(($) => $.passkeys.revoke_dialog.confirm)}
            </AlertDialogAction>
            {hasOtherSigner && (
              <AlertDialogAction
                variant="outline"
                onClick={async () => {
                  const id = revokeId;
                  setRevokeId(null);
                  if (id) await revoke(id, true);
                }}
              >
                {t(($) => $.passkeys.revoke_dialog.use_code)}
              </AlertDialogAction>
            )}
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </SettingsTab>
  );
}
