import assert from "node:assert/strict";
import test from "node:test";

import { copyHits, flatten, localeErrors, normalise } from "./check-copy.mjs";

// --- locale key rule (blocking) -------------------------------------------------

const tree = (en, fr = en) => ({ en, fr });

test("locale rule: key removed in en but kept in fr fails", () => {
  const base = tree({ chat: { a: "A", b: "B" } });
  const head = { en: { chat: { a: "A" } }, fr: { chat: { a: "A", b: "B" } } };
  assert.match(localeErrors(base, head).join("\n"), /chat:b .*removed or renamed in en.*fr/);
});

test("locale rule: key renamed in en but old key kept in fr fails", () => {
  const base = tree({ chat: { old: "X" } });
  const head = { en: { chat: { fresh: "X" } }, fr: { chat: { old: "X" } } };
  assert.match(localeErrors(base, head).join("\n"), /chat:old .*fr/);
});

test("locale rule: key removed in every locale passes", () => {
  const base = tree({ chat: { a: "A", b: "B" } });
  const head = tree({ chat: { a: "A" } });
  assert.deepEqual(localeErrors(base, head), []);
});

test("locale rule: unchanged en passes; key new in en is not this rule's job", () => {
  const base = tree({ chat: { a: "A" } });
  const head = { en: { chat: { a: "A", b: "B" } }, fr: { chat: { a: "A" } } };
  assert.deepEqual(localeErrors(base, head), []);
});

test("locale rule: plural forms compare as one key (zh has only _other)", () => {
  const base = { en: { c: { n_one: "1", n_other: "2" } }, zh: { c: { n_other: "2" } } };
  const head = { en: { c: {} }, zh: { c: {} } };
  assert.deepEqual(localeErrors(base, head), []);
  const kept = { en: { c: {} }, zh: { c: { n_other: "2" } } };
  assert.match(localeErrors(base, kept).join("\n"), /c:n_count .*zh/);
});

// --- copy check (advisory) ------------------------------------------------------

const refs = [
  { pattern: "inline-retry", key: "chat:needs_you.retry", value: "Retry" },
  { pattern: "attachment-card-list", key: "issues:deliverables.version_short", value: "v{{version}}" },
  { pattern: "reply-annotation", key: "issues:reply.annotations.add", value: "Add annotation" },
];
const hits = (value, key = "x:new") => {
  const [ns, k] = key.split(":");
  return copyHits([{ ns, key: k, value }], refs);
};

test("copy: exact duplicate after normalising case and punctuation", () => {
  const [h] = hits("  retry! ");
  assert.equal(h.pattern, "inline-retry");
  assert.equal(h.kind, "exact");
});

test("copy: synonym duplicate (comment, reply, annotation, note)", () => {
  for (const word of ["Add note", "Add comment", "Add reply"]) {
    const [h] = hits(word);
    assert.equal(h?.pattern, "reply-annotation", word);
    assert.equal(h.kind, "synonym");
  }
});

test("copy: unrelated label does not match", () => {
  assert.deepEqual(hits("Delete workspace"), []);
  assert.deepEqual(hits("Retry later"), []);
  assert.deepEqual(hits("…"), []);
});

test("copy: the pattern's own key is never a hit", () => {
  assert.deepEqual(hits("Retry", "chat:needs_you.retry"), []);
});

test("normalise and flatten", () => {
  assert.equal(normalise("Try again…"), "retry");
  assert.deepEqual([...flatten({ a: { b: "x", c: 1 }, d: "y" })], [["a.b", "x"], ["d", "y"]]);
});

// --- S0b seeded duplicates (plan section 3: precision and miss-rate fixtures) -------
// Values copied from `packages/views/locales/en` on main b29d81efcc (design/audit/ui-drift.md, S3 table).
// `caught` is the measured outcome today. A seed that is missed documents a known limit of the exact-match rule.
const ret = { pattern: "inline-retry", key: "chat:needs_you.retry", value: "Retry" };
const slack = (k, value) => ({ pattern: "integration-tab", key: `settings:slack.${k}`, value });
const seeds = [
  { n: 1, ns: "issues", key: "x.try_again", value: "Try again", ref: ret, caught: true },
  { n: 2, ns: "chat", key: "scheduled.load_failed", value: "Couldn't load scheduled follow-ups. Retry", ref: ret, caught: false },
  { n: 3, ns: "settings", key: "dingtalk.revoked_badge", value: "revoked", ref: slack("revoked_badge", "revoked"), caught: true },
  { n: 4, ns: "settings", key: "lark.installed_at_label", value: "Installed {{when}}", ref: slack("installed_at_label", "Installed {{when}}"), caught: true },
  { n: 5, ns: "settings", key: "lark.not_enabled_title", value: "Lark integration not enabled", ref: slack("not_enabled_title", "Slack integration not enabled"), caught: false },
  { n: 6, ns: "settings", key: "lark.empty_title", value: "No bots connected yet", ref: slack("empty_title", "No bots connected yet"), caught: true },
  { n: 7, ns: "settings", key: "lark.toast_disconnected", value: "Disconnected Lark bot", ref: slack("toast_disconnected", "Disconnected Slack bot"), caught: false },
  { n: 8, ns: "settings", key: "dingtalk.agent_bot_connected_label", value: "Connected bot:", ref: slack("agent_bot_connected_label", "Connected to Slack"), caught: false },
  { n: 9, ns: "issues", key: "attachment_revision.badge", value: "v{{revision}} · {{earlier}} earlier", ref: { pattern: "attachment-card-list", key: "issues:deliverables.version_short", value: "v{{version}}" }, caught: false },
  { n: 10, ns: "invite", key: "not_found.title", value: "Invitation not found", ref: { pattern: "state-card", key: "common:not_found.title", value: "Page not found" }, caught: false },
];

for (const s of seeds) {
  test(`seed ${s.n}: ${s.caught ? "caught" : "missed (known limit)"}`, () => {
    const got = copyHits([{ ns: s.ns, key: s.key, value: s.value }], [s.ref]);
    assert.equal(got.length > 0, s.caught);
  });
}

test("seed summary: caught count is 4 of 10", () => {
  const caught = seeds.filter((s) => copyHits([{ ns: s.ns, key: s.key, value: s.value }], [s.ref]).length).length;
  assert.equal(caught, 4);
});
