export * from "../../../../../packages/core/permissions/index";

// Role comes from the page URL (?role=owner|admin|member), so one build shows every viewer.
export function useCurrentMember() {
  const role = new URLSearchParams(location.search).get("role");
  return { userId: "capture-user", role: (role ?? "owner") as "owner" | "admin" | "member", member: null, isLoading: false };
}
