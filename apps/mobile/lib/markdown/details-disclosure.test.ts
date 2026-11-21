import { describe, expect, it } from "vitest";
import { detailsBodyFor } from "./details-disclosure";
import type { DetailsSegment } from "./split-markdown";

const AGENT_DID = "Ran all requested checks.";
const segment: DetailsSegment = {
  type: "details",
  summary: "Details",
  body: [{ type: "prose", content: `**Agent did**\n${AGENT_DID}` }],
};

describe("detailsBodyFor", () => {
  it("returns null while collapsed", () => {
    expect(detailsBodyFor(segment, false)).toBeNull();
  });

  it("returns the body segments when expanded", () => {
    const body = detailsBodyFor(segment, true);
    expect(body).not.toBeNull();
    expect(JSON.stringify(body)).toContain(AGENT_DID);
  });
});
