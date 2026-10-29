// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { proxySearchApi } from "./search-proxy";

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
});

describe("search API proxy cancellation", () => {
  it("returns 499 without logging when the request is aborted", async () => {
    vi.stubEnv("REMOTE_API_URL", "http://backend:8080");
    const controller = new AbortController();
    const request = new Request("http://web.test/api/search/suggestions?q=typed", {
      signal: controller.signal,
    });
    const fetchMock = vi.fn(
      (_input: RequestInfo | URL, options?: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          const signal = options?.signal;
          if (!signal) {
            reject(new Error("The request signal was not forwarded"));
            return;
          }
          if (signal.aborted) {
            reject(new DOMException("The operation was aborted", "AbortError"));
            return;
          }
          signal.addEventListener(
            "abort",
            () => reject(new DOMException("The operation was aborted", "AbortError")),
            { once: true },
          );
        }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => undefined);

    const responsePromise = proxySearchApi(request, "/api/search/suggestions");
    controller.abort();
    const response = await responsePromise;

    expect(response.status).toBe(499);
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(fetchMock).toHaveBeenCalledWith(
      new URL("http://backend:8080/api/search/suggestions?q=typed"),
      expect.objectContaining({ signal: request.signal }),
    );
    expect(errorSpy).not.toHaveBeenCalled();
  });
});
