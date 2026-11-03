import { describe, expect, it } from "vitest";
import { z } from "zod";
import * as schemas from "./schemas";

// CHE-3019: building ~200 zod schemas at module load costs ~0.3 s of main
// thread per page load (4x throttle: >1 s). Every exported schema must defer
// its construction to the first parse.
describe("api schemas", () => {
  it("construct nothing at module load", () => {
    const eager = Object.entries(schemas)
      .filter(([, v]) => v instanceof z.ZodType)
      .filter(([, v]) => (v as z.ZodType).def.type !== "lazy")
      .map(([name]) => name);
    expect(eager).toEqual([]);
  });
});
