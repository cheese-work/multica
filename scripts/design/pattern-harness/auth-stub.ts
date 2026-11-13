// The harness has no session: the signed-in viewer is a fixed workspace owner.
export const useAuthStore = <T,>(select: (state: { user: { id: string } }) => T): T => select({ user: { id: "u-owner" } });
