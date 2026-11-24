"use client";

import { useState } from "react";
import { Check, Copy } from "@multica/ui/icons";
import { CliInstallCommand } from "@multica/views/common/cli-install-command";
import { copyText } from "@multica/ui/lib/clipboard";
import { cn } from "@multica/ui/lib/utils";
import { useLocale } from "../../i18n";
import { Container, Eyebrow, focusRing } from "../../site/primitives";

const SETUP_CMD = "multica setup";

/**
 * The landing palette bypasses the product tokens, so the shared platform
 * switch takes its list and trigger colours from here.
 */
const PLATFORM_TABS = {
  list: "border border-white/10 bg-white/5",
  trigger:
    "text-site-inverse-muted hover:text-site-inverse-ink data-active:bg-white/10 data-active:text-site-inverse-ink",
};

/**
 * For servers and remote dev boxes: the CLI runs the same daemon as the
 * desktop app. The install command carries the platform switch, since
 * install.sh and install.ps1 differ.
 */
export function CliSection() {
  const { t } = useLocale();
  const d = t.download.cli;

  return (
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
    <section id="cli" className="bg-[#f7f7f5] py-20 text-[#0a0d12] sm:py-24">
      <div className="mx-auto max-w-[820px] px-4 sm:px-6 lg:px-8">
        <h2 className="landing-serif text-[2.2rem] leading-[1.1] tracking-[-0.03em] sm:text-[2.6rem]">
          {d.title}
        </h2>
        <p className="mt-4 max-w-[620px] text-body-lg leading-7 text-[#0a0d12]/72">
          {d.sub}
        </p>

        <div className="mt-10 flex flex-col gap-5">
          <div>
            <CommandLabel>{d.installLabel}</CommandLabel>
            <CliInstallCommand
              labels={{
                group: d.platformGroup,
                macosLinux: d.platformMacosLinux,
              }}
              classNames={PLATFORM_TABS}
            >
              {(cmd) => (
                <CommandRow
                  cmd={cmd}
                  copyLabel={d.copyLabel}
                  copiedLabel={d.copiedLabel}
                />
              )}
            </CliInstallCommand>
          </div>
          <div>
            <CommandLabel>{d.startLabel}</CommandLabel>
            <CommandRow
              cmd={SETUP_CMD}
              copyLabel={d.copyLabel}
              copiedLabel={d.copiedLabel}
            />
          </div>
||||||| 2ae2f08005
    <section id="cli" className="bg-[#f7f7f5] py-20 text-[#0a0d12] sm:py-24">
      <div className="mx-auto max-w-[820px] px-4 sm:px-6 lg:px-8">
        <h2 className="landing-serif text-[2.2rem] leading-[1.1] tracking-[-0.03em] sm:text-[2.6rem]">
          {d.title}
        </h2>
        <p className="mt-4 max-w-[620px] text-body-lg leading-7 text-[#0a0d12]/72">
          {d.sub}
        </p>

        <div className="mt-10 flex flex-col gap-5">
          <div>
            <CommandLabel>{d.installLabel}</CommandLabel>
            <CliInstallCommand
              labels={{
                group: d.platformGroup,
                macosLinux: d.platformMacosLinux,
                windows: d.platformWindows,
              }}
              classNames={PLATFORM_TABS}
            >
              {(cmd) => (
                <CommandRow
                  cmd={cmd}
                  copyLabel={d.copyLabel}
                  copiedLabel={d.copiedLabel}
                />
              )}
            </CliInstallCommand>
          </div>
          <div>
            <CommandLabel>{d.startLabel}</CommandLabel>
            <CommandRow
              cmd={SETUP_CMD}
              copyLabel={d.copyLabel}
              copiedLabel={d.copiedLabel}
            />
          </div>
=======
    <section id="cli" className="border-t border-site-rule py-site-section">
      <Container className="grid gap-8 lg:grid-cols-[5fr_7fr] lg:items-center lg:gap-16">
        <div>
          <Eyebrow>CLI</Eyebrow>
          <h2 className="mt-3 font-site-display text-site-title text-site-ink text-balance">{d.title}</h2>
          <p className="mt-3 max-w-[30rem] text-site-body text-site-muted text-pretty">{d.sub}</p>
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
        </div>
        <div className="min-w-0 rounded-site-panel bg-site-inverse p-5 text-site-inverse-ink">
          <CliInstallCommand
            labels={{ group: d.platformGroup, macosLinux: d.platformMacosLinux, windows: d.platformWindows }}
            classNames={PLATFORM_TABS}
          >
            {(cmd) => <CommandRow label={d.installLabel} cmd={cmd} />}
          </CliInstallCommand>
          <CommandRow label={d.startLabel} cmd={SETUP_CMD} />
        </div>
      </Container>
    </section>
  );
}

function CommandRow({ label, cmd }: { label: string; cmd: string }) {
  const { copyLabel, copiedLabel } = useLocale().t.download.cli;
  const [copied, setCopied] = useState(false);

  const onCopy = async () => {
    if (await copyText(cmd)) {
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    }
  };

  return (
    <div className="mt-4">
      <p className="font-site-mono text-[0.75rem] text-site-inverse-muted"># {label}</p>
      <div className="mt-1 flex items-start gap-3 font-site-mono text-[0.8125rem] leading-6">
        <span aria-hidden className="text-site-inverse-muted">
          $
        </span>
        <code className="min-w-0 flex-1 break-all">{cmd}</code>
        <button
          type="button"
          onClick={onCopy}
          aria-label={copied ? copiedLabel : copyLabel}
          className={cn(
            "flex size-6 shrink-0 items-center justify-center rounded-site-inner text-site-inverse-muted transition-colors hover:bg-white/10 hover:text-site-inverse-ink",
            focusRing,
            "focus-visible:outline-site-inverse-ink",
          )}
        >
          {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
        </button>
      </div>
    </div>
  );
}
