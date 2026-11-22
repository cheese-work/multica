// Security regression for the pinned `decode-uri-component@0.2.2` (CHE-4844,
// GHSA-qr28-fpjm-wq7f quadratic replace, GHSA-vcc3-ghjq-m6fr exponential
// decode). The package is patched in place (root `patches/`), so this test
// loads it through the same `require` chain Metro uses: expo-router ->
// query-string -> decode-uri-component. It must keep passing if the patch or
// the lockfile pin is dropped, so it fails (not hangs) on a time budget.
import { createRequire } from "node:module";
import { Worker } from "node:worker_threads";
import { describe, expect, it } from "vitest";

const fromHere = createRequire(import.meta.url);
const fromExpoRouter = createRequire(fromHere.resolve("expo-router/package.json"));
const fromQueryString = createRequire(fromExpoRouter.resolve("query-string/package.json"));
const decodeModulePath = fromQueryString.resolve("decode-uri-component");
const decode = fromQueryString("decode-uri-component") as (s: string) => string;

const BUDGET_MS = 1000;

/** Advisory PoC: 32,000 distinct `%HH%LLx` runs plus a trailing `%zz` (224,003 chars). */
function pocInput(): string {
  let s = "";
  for (let i = 0; i < 32_000; i++) {
    const hh = (i >> 8).toString(16).padStart(2, "0");
    const ll = (i & 255).toString(16).padStart(2, "0");
    s += `%${hh}%${ll}x`;
  }
  return `${s}%zz`;
}

// The unpatched decoder takes ~20-40 s here. Run it in a worker so a miss
// terminates at the budget instead of hanging the suite.
function decodeWithinBudget(input: string): Promise<"ok" | "timeout"> {
  return new Promise((resolve, reject) => {
    const worker = new Worker(
      `const {workerData:{p,s},parentPort}=require("node:worker_threads");require(p)(s);parentPort.postMessage("ok")`,
      { eval: true, workerData: { p: decodeModulePath, s: input } },
    );
    const timer = setTimeout(() => {
      void worker.terminate();
      resolve("timeout");
    }, BUDGET_MS);
    worker.once("message", () => {
      clearTimeout(timer);
      void worker.terminate();
      resolve("ok");
    });
    worker.once("error", (err) => {
      clearTimeout(timer);
      reject(err);
    });
  });
}

describe("decode-uri-component 0.2.2 (patched)", () => {
  it("decodes the advisory proof of concept within 1 s", async () => {
    expect(await decodeWithinBudget(pocInput())).toBe("ok");
  });

  // Vectors from test.js at tag v0.2.2 of SamVerschueren/decode-uri-component:
  // the patch must not change any output.
  const vectors: Record<string, string> = {
    test: "test",
    "a+b": "a b",
    "a+b+c+d": "a b c d",
    "=a": "=a",
    "%": "%",
    "%25": "%",
    "%%25%%": "%%%%",
    "st%C3%A5le": "ståle",
    "st%C3%A5le%": "ståle%",
    "%st%C3%A5le%": "%ståle%",
    "%%7Bst%C3%A5le%7D%": "%{ståle}%",
    "%ab%C3%A5le%": "%abåle%",
    "%C3%A5%able%": "å%able%",
    "%7B%ab%7C%de%7D": "{%ab|%de}",
    "%7B%ab%%7C%de%%7D": "{%ab%|%de%}",
    "%7 B%ab%%7C%de%%7 D": "%7 B%ab%|%de%%7 D",
    "%ab": "%ab",
    "%ab%ab%ab": "%ab%ab%ab",
    "%61+%4d%4D": "a MM",
    "﻿test": "﻿test",
    "﻿": "﻿",
    "%EF%BB%BFtest": "﻿test",
    "%EF%BB%BF": "﻿",
    "%FE%FF": "��",
    "%FF%FE": "��",
    "†": "†",
    "%C2": "�",
    "%C2x": "�x",
    "%C2%B5": "µ",
    "%C2%B5%": "µ%",
    "%%C2%B5%": "%µ%",
    "%ea%ba%5a%ba": "%ea%baZ%ba",
    "%C3%5A%A5": "%C3Z%A5",
    "%C3%5A%A5%AB": "%C3Z%A5%AB",
  };

  it.each(Object.entries(vectors))("v0.2.2 vector %j -> %j", (input, expected) => {
    expect(decode(input)).toBe(expected);
  });

  it("throws TypeError for a non-string", () => {
    expect(() => decode(5 as unknown as string)).toThrow(
      "Expected `encodedURI` to be of type `string`, got `number`",
    );
  });
});
