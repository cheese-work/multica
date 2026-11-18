import assert from "node:assert/strict";
import test from "node:test";

import { buildKey } from "./keys.mjs";

const profiles = [
  { id: "web-1440", themes: ["light", "dark"] },
  { id: "phone-358", themes: ["dark"] },
];
const part = (over = {}) => ({ source: "route", screen: "ws.issues", state: "default", profile: "web-1440", theme: "dark", ...over });
const codeOf = (fn) => {
  try {
    fn();
  } catch (e) {
    return e.code;
  }
  return undefined;
};

test("key is <source>/<screen>/<state>/<profile>/<theme> (C1 Live key)", () => {
  assert.equal(buildKey(part(), profiles), "route/ws.issues/default/web-1440/dark");
  assert.equal(
    buildKey(part({ state: "tab-members", theme: "light" }), profiles),
    "route/ws.issues/tab-members/web-1440/light",
  );
});
test("unknown profile is unknown-profile", () => {
  assert.equal(codeOf(() => buildKey(part({ profile: "tablet-9" }), profiles)), "unknown-profile");
});
test("a theme the profile does not list is unknown-theme", () => {
  assert.equal(codeOf(() => buildKey(part({ profile: "phone-358", theme: "light" }), profiles)), "unknown-theme");
  assert.equal(codeOf(() => buildKey(part({ theme: "sepia" }), profiles)), "unknown-theme");
});
test("source must be route or scene; empty parts and slashes are refused", () => {
  assert.equal(codeOf(() => buildKey(part({ source: "page" }), profiles)), "invalid-key-part");
  assert.equal(codeOf(() => buildKey(part({ screen: "" }), profiles)), "invalid-key-part");
  assert.equal(codeOf(() => buildKey(part({ state: "a/b" }), profiles)), "invalid-key-part");
});
