// CHE-7337: the title tier answers each keystroke; the full ranked search follows
// the pause (or Enter) and replaces the rows in place.
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const { mockApi, mockFull, mockProjects, mockLocalReady } = vi.hoisted(() => ({
  mockApi: { searchIssues: vi.fn() },
  mockFull: vi.fn(),
  mockProjects: vi.fn(),
  mockLocalReady: vi.fn(() => false),
}));

vi.mock("@multica/core/api", () => ({ api: mockApi }));
vi.mock("@multica/core/search-index", () => ({
  isLocalSearchReady: mockLocalReady,
  searchIssues: mockFull,
  searchProjects: mockProjects,
}));

import { useServerSearch } from "./use-server-search";

const issue = (id: string) => ({ id, title: id }) as never;
const settle = () => act(async () => {});

describe("useServerSearch title tier", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mockLocalReady.mockReturnValue(false);
    mockApi.searchIssues.mockReset().mockResolvedValue({ issues: [issue("t1")] });
    mockFull.mockReset().mockResolvedValue({ issues: [issue("t2")] });
    mockProjects.mockReset().mockResolvedValue({ projects: [] });
  });
  afterEach(() => vi.useRealTimers());

  it("shows title rows on the keystroke, before the debounce", async () => {
    const { result } = renderHook(() => useServerSearch("fuzzy", { projects: true }));
    await settle();

    expect(mockApi.searchIssues).toHaveBeenCalledWith(
      expect.objectContaining({ q: "fuzzy", limit: 8, include_closed: true, title_only: true }),
    );
    expect(mockFull).not.toHaveBeenCalled();
    expect(result.current.results.issues.map((i) => i.id)).toEqual(["t1"]);
    expect(result.current.results.query).toBe("fuzzy");
    expect(result.current.stale).toBe(false);
  });

  it("replaces the title rows with the full search after the pause", async () => {
    const { result } = renderHook(() => useServerSearch("fuzzy", { projects: true }));
    await settle();
    await act(async () => void vi.advanceTimersByTime(300));

    expect(mockFull).toHaveBeenCalledWith(expect.objectContaining({ q: "fuzzy", limit: 20 }));
    expect(result.current.results.issues.map((i) => i.id)).toEqual(["t2"]);
  });

  it("runs the full search at once on searchNow (Enter)", async () => {
    const { result } = renderHook(() => useServerSearch("fuzzy", { projects: true }));
    await settle();
    act(() => result.current.searchNow());
    await settle();

    expect(mockFull).toHaveBeenCalledTimes(1);
    expect(result.current.results.issues.map((i) => i.id)).toEqual(["t2"]);
  });

  it("ignores a title response that lands after the full search", async () => {
    let resolveTitle!: (v: unknown) => void;
    mockApi.searchIssues.mockReturnValue(new Promise((r) => (resolveTitle = r)));
    const { result } = renderHook(() => useServerSearch("fuzzy", { projects: true }));
    await act(async () => void vi.advanceTimersByTime(300));
    expect(result.current.results.issues.map((i) => i.id)).toEqual(["t2"]);

    await act(async () => resolveTitle({ issues: [issue("t1")] }));
    expect(result.current.results.issues.map((i) => i.id)).toEqual(["t2"]);
  });

  it("still delivers the full search when the title tier fails", async () => {
    mockApi.searchIssues.mockRejectedValue(new Error("boom"));
    const { result } = renderHook(() => useServerSearch("fuzzy", { projects: true }));
    await settle();
    expect(result.current.results.issues).toEqual([]);
    await act(async () => void vi.advanceTimersByTime(300));

    expect(result.current.results.issues.map((i) => i.id)).toEqual(["t2"]);
  });

  // CHE-7423: the title tier owns issues only, so each keystroke's title
  // response used to wipe the project group until the full search came back.
  it("keeps the project rows that still match while the next title tier lands", async () => {
    const project = (id: string, title: string) => ({ id, title }) as never;
    mockProjects.mockResolvedValue({
      projects: [project("p1", "Fuzzy search rollout"), project("p2", "Fuzzy matching lab")],
    });
    const { result, rerender } = renderHook(({ q }) => useServerSearch(q, { projects: true }), {
      initialProps: { q: "fuzzy" },
    });
    await settle();
    await act(async () => void vi.advanceTimersByTime(300));
    expect(result.current.results.projects.map((p) => p.id)).toEqual(["p1", "p2"]);

    mockApi.searchIssues.mockResolvedValue({ issues: [issue("t3")] });
    rerender({ q: "fuzzy s" });
    await settle();

    // Title tier for "fuzzy s" is on screen; the project that still matches stays.
    expect(result.current.results.issues.map((i) => i.id)).toEqual(["t3"]);
    expect(result.current.results.projects.map((p) => p.id)).toEqual(["p1"]);
  });

  it("keeps today's behavior while the local index serves", async () => {
    mockLocalReady.mockReturnValue(true);
    const { result } = renderHook(() => useServerSearch("fuzzy", { projects: true }));
    await act(async () => void vi.advanceTimersByTime(0));
    await settle();

    expect(mockApi.searchIssues).not.toHaveBeenCalled();
    expect(mockFull).toHaveBeenCalledTimes(1);
    expect(result.current.results.issues.map((i) => i.id)).toEqual(["t2"]);
  });
});
