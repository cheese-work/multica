// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { flagMentions } from "./flag-mentions";

const root = () => {
  const el = document.createElement("div");
  el.innerHTML =
    '<span class="mention" data-mention-id="abc">@Ghost</span><span class="mention" data-mention-id="def">@Bob</span>';
  return el;
};

describe("flagMentions", () => {
  it("flags only the matching chips and clears earlier flags", () => {
    const el = root();
    flagMentions(el, ["abc"]);
    expect([...el.querySelectorAll(".mention-invalid")].map((e) => e.textContent)).toEqual(["@Ghost"]);
    flagMentions(el, ["def"]);
    expect([...el.querySelectorAll(".mention-invalid")].map((e) => e.textContent)).toEqual(["@Bob"]);
    flagMentions(el, []);
    expect(el.querySelector(".mention-invalid")).toBeNull();
  });
});
