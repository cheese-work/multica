// The harness has no session: the signed-in viewer is a fixed workspace owner.
export const useAuthStore = <T,>(select: (state: { user: { id: string } }) => T): T => select({ user: { id: "u-owner" } });
// raw-elements.tsx (S0b) registers a store on the real module; the harness reads the fixed viewer above, so this is a no-op.
export const registerAuthStore = (_store: unknown): void => {};
