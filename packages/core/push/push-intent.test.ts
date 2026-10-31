// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { stubBrowserStorage, type FakeBrowserStorage } from "./fake-browser-storage";

type Intent = typeof import("./push-intent");

// A page load: the module is evaluated again, only browser storage survives.
async function freshPage(): Promise<Intent> {
  vi.resetModules();
  return import("./push-intent");
}

let storage: FakeBrowserStorage;

beforeEach(() => {
  storage = stubBrowserStorage();
});

afterEach(() => vi.unstubAllGlobals());

describe("push intent", () => {
  it("is unset until a choice is recorded", async () => {
    const page = await freshPage();
    await expect(page.getPushIntent()).resolves.toBeNull();
  });

  it("survives a reload once turned on", async () => {
    await (await freshPage()).setPushIntent("on");
    await expect((await freshPage()).getPushIntent()).resolves.toBe("on");
  });

  it("survives a reload once turned off", async () => {
    const before = await freshPage();
    await before.setPushIntent("on");
    await before.setPushIntent("off");
    await expect((await freshPage()).getPushIntent()).resolves.toBe("off");
  });

  it("turning on again after an off lifts the off marker", async () => {
    const page = await freshPage();
    await page.setPushIntent("off");
    await page.setPushIntent("on");
    expect(storage.cookies.size).toBe(0);
    await expect((await freshPage()).getPushIntent()).resolves.toBe("on");
  });

  it("ignores a value it did not write", async () => {
    storage.idb.set("intent", "garbage");
    await expect((await freshPage()).getPushIntent()).resolves.toBeNull();
  });

  it("never throws and reports nothing where storage is unavailable", async () => {
    storage.idbMode = "unavailable";
    const page = await freshPage();
    await expect(page.setPushIntent("on")).resolves.toBeUndefined();
    await expect(page.setPushIntent("off")).resolves.toBeUndefined();
    await expect((await freshPage()).getPushIntent()).resolves.not.toBe("on");
  });

  describe("an opt-out that storage cannot fully record", () => {
    async function optOutWith(mode: Partial<Pick<FakeBrowserStorage, "idbMode" | "cookieMode">>) {
      const page = await freshPage();
      await page.setPushIntent("on");
      Object.assign(storage, mode);
      await page.setPushIntent("off");
      return page;
    }

    it("holds after a reload when the database is full: the record is deleted, not overwritten", async () => {
      await optOutWith({ idbMode: "no-space" });
      await expect((await freshPage()).getPushIntent()).resolves.not.toBe("on");
    });

    it("holds after a reload when the database is read-only and still says on: the cookie marks the opt-out", async () => {
      await optOutWith({ idbMode: "read-only" });
      expect(storage.idb.get("intent")).toBe("on");
      await expect((await freshPage()).getPushIntent()).resolves.toBe("off");
    });

    it("holds after a reload when only the cookie is refused", async () => {
      await optOutWith({ cookieMode: "denied" });
      await expect((await freshPage()).getPushIntent()).resolves.not.toBe("on");
    });

    it("holds for this page when nothing could be written", async () => {
      const page = await optOutWith({ idbMode: "read-only", cookieMode: "denied" });
      await expect(page.getPushIntent()).resolves.toBe("off");
    });

    it("is not revived by an unreadable database on the next page", async () => {
      await optOutWith({ idbMode: "read-only", cookieMode: "denied" });
      storage.idbMode = "unavailable";
      await expect((await freshPage()).getPushIntent()).resolves.not.toBe("on");
    });

    it("lets a later successful write replace the page's unsaved choice", async () => {
      const page = await optOutWith({ idbMode: "read-only", cookieMode: "denied" });
      Object.assign(storage, { idbMode: "ok", cookieMode: "ok" });
      await page.setPushIntent("on");
      await expect(page.getPushIntent()).resolves.toBe("on");
    });
  });

  it("does not report on when the database refuses to record it", async () => {
    storage.idbMode = "read-only";
    const page = await freshPage();
    await page.setPushIntent("on");
    await expect(page.getPushIntent()).resolves.toBe("on");
    await expect((await freshPage()).getPushIntent()).resolves.not.toBe("on");
  });

  // Documented residual (CHE-1798 round 5, finding 2): nothing on the client can record an
  // opt-out when the database refuses deletes too and cookies are refused. This pins the
  // behaviour so a change to it is deliberate; closing it needs the choice stored on the server.
  it("known residual: a fresh page reads a stale on when the database refuses deletes and cookies are refused", async () => {
    const page = await freshPage();
    await page.setPushIntent("on");
    Object.assign(storage, { idbMode: "read-only", cookieMode: "denied" });
    await page.setPushIntent("off");
    await expect(page.getPushIntent()).resolves.toBe("off");
    await expect((await freshPage()).getPushIntent()).resolves.toBe("on");
  });
});
