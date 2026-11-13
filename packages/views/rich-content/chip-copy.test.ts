/**
 * Copying a selection that contains inline chips writes the chip's key, not
 * its rendered label (CHE-3371).
 */
import { afterEach, describe, expect, it } from "vitest";
import { copyChipKeys } from "./chip-copy";

const ISSUE = (id: string, label: string) =>
  `<a href="/ws/issues/${id}"><span data-copy-token="${id}">${id} · ${label}</span></a>`;
const COMMENT = `<a href="/ws/issues/uuid-1#comment-c1"><span data-copy-token="CHE-3229#comment-c1">CHE-3229 · bot: hi</span></a>`;
const PROJECT = `<a href="/ws/projects/p1"><span data-copy-token="Roadmap">🗺 Roadmap</span></a>`;

function copyAll(html: string, partial?: (root: HTMLElement) => Range) {
  const root = document.createElement("div");
  root.innerHTML = html;
  document.body.appendChild(root);
  const range = partial ? partial(root) : (() => {
    const r = document.createRange();
    r.selectNodeContents(root);
    return r;
  })();
  const sel = window.getSelection()!;
  sel.removeAllRanges();
  sel.addRange(range);
  const data: Record<string, string> = {};
  let prevented = false;
  const handled = copyChipKeys({
    clipboardData: { setData: (t: string, v: string) => (data[t] = v) },
    preventDefault: () => (prevented = true),
  } as unknown as ClipboardEvent);
  return { handled, prevented, data };
}

afterEach(() => {
  document.body.innerHTML = "";
});

describe("copyChipKeys", () => {
  it("copies an issue chip as its identifier, linked in html", () => {
    const { data, prevented } = copyAll(
      `<p>see ${ISSUE("CHE-3229", "fix(security-gate): cso…")} for details</p>`,
    );
    expect(prevented).toBe(true);
    expect(data["text/plain"]).toBe("see CHE-3229 for details");
    expect(data["text/html"]).toContain('<a href="http://localhost:3000/ws/issues/CHE-3229">CHE-3229</a>');
    expect(data["text/html"]).not.toContain("fix(security-gate)");
  });

  it("copies a comment chip as identifier#comment-id and a project chip as its title", () => {
    const { data } = copyAll(`<p>${COMMENT} and ${PROJECT}</p>`);
    expect(data["text/plain"]).toBe("CHE-3229#comment-c1 and Roadmap");
  });

  it("keeps text outside chips and line breaks", () => {
    const { data } = copyAll(
      `<p>one ${ISSUE("CHE-1", "a")}</p><p>two<br>three ${ISSUE("CHE-2", "b")} four</p>`,
    );
    expect(data["text/plain"]).toBe("one CHE-1\n\ntwo\nthree CHE-2 four");
  });

  it("does not collapse or trim newlines in text outside chips", () => {
    const { data } = copyAll(`<pre>a\n\n\nb ${ISSUE("CHE-1", "x")}\n\n</pre>`);
    expect(data["text/plain"]).toBe("a\n\n\nb CHE-1\n\n");
  });

  it("copies the token when the selection is fully inside one chip", () => {
    const { data } = copyAll(`<p>x ${ISSUE("CHE-9", "title text")} y</p>`, (root) => {
      const text = root.querySelector("[data-copy-token]")!.firstChild!;
      const r = document.createRange();
      r.setStart(text, 3);
      r.setEnd(text, 8);
      return r;
    });
    expect(data["text/plain"]).toBe("CHE-9");
  });

  it("leaves the default copy alone when no chip is selected", () => {
    const { handled, prevented, data } = copyAll(`<p>plain ${ISSUE("CHE-1", "a")}</p>`, (root) => {
      const text = root.querySelector("p")!.firstChild!;
      const r = document.createRange();
      r.setStart(text, 0);
      r.setEnd(text, 5);
      return r;
    });
    expect(handled).toBe(false);
    expect(prevented).toBe(false);
    expect(data).toEqual({});
  });
});
