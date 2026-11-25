// @vitest-environment node

import { describe, expect, it } from "vitest";
import { SUPPORTED_LOCALES } from "@multica/core/i18n";
import { RESOURCES } from "./index";

// The CLI only accepts a loopback callback host (CHE-9626), so the recovery
// text shown for a rejected callback must send remote users down the SSH
// port-forward route and never recommend a LAN / RFC 1918 address.
describe("web.cli_callback.invalid_description", () => {
  it.each(SUPPORTED_LOCALES)(
    "names the ssh -L route and no LAN address in %s",
    (locale) => {
      const text = (
        RESOURCES[locale].auth as {
          web: { cli_callback: { invalid_description: string } };
        }
      ).web.cli_callback.invalid_description;

      expect(text).toContain("--callback-host 127.0.0.1");
      expect(text).toContain("ssh -L PORT:127.0.0.1:PORT USER@CLI_MACHINE");
      expect(text).not.toMatch(/RFC\s?1918|\bLAN\b|192\.168|\b10\.\d+\.\d+\.\d+/);
    },
  );
});
