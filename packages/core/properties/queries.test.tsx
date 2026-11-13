/**
 * @vitest-environment jsdom
 */
import { describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { createQueryClient } from "../query-client";
import { propertyListOptions } from "./queries";

const live = { id: "p-live", name: "Live" };
const archived = { id: "p-old", name: "Old", archived: true };

describe("propertyListOptions", () => {
  // The issue detail page needs archived definitions; the surfaces beside it
  // need only live ones. Both read one catalog, so one request serves both.
  it("serves live-only and archived-inclusive readers from one request", async () => {
    const listProperties = vi.fn(async (includeArchived?: boolean) => {
      const properties = includeArchived ? [live, archived] : [live];
      return { properties, total: properties.length };
    });
    setApiInstance({ listProperties } as unknown as ApiClient);
    const qc = createQueryClient();
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    );

    const { result } = renderHook(
      () => ({
        liveOnly: useQuery(propertyListOptions("ws-1")),
        withArchived: useQuery(propertyListOptions("ws-1", true)),
      }),
      { wrapper },
    );

    await waitFor(() => expect(result.current.withArchived.data).toHaveLength(2));
    expect(result.current.liveOnly.data?.map((p) => p.id)).toEqual(["p-live"]);
    expect(listProperties).toHaveBeenCalledTimes(1);
    qc.clear();
  });
});
