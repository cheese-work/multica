// Harness-only (CHE-5532): the real module pulls the auth store, which hangs headless Chrome at init.
export const useCurrentMember = () => ({ role: "member" as const, isLoading: false });
export const useAgentPermissions = () => ({});
export const useSkillPermissions = () => ({});
export const canAssignAgentToIssue = () => ({ allowed: true });
export const canEditAgent = () => ({ allowed: true });
