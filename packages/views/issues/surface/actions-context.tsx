"use client";

import { createContext, useContext, type ReactNode } from "react";
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
import type { BatchUpdateIssuesResponse, Issue, UpdateIssueRequest } from "@multica/core/types";
||||||| 2ae2f08005
import type { Issue, UpdateIssueRequest } from "@multica/core/types";
=======
import type {
  BatchIssueLabelOps,
  BatchIssuePropertyOps,
  BatchUpdateIssuesResponse,
  Issue,
  UpdateIssueRequest,
} from "@multica/core/types";
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
import type { IssueCreateDefaults } from "./types";

export type IssueSurfaceMutationOptions = {
  errorMessage?: string;
  onSuccess?: (issue: Issue) => void;
  onError?: (err: unknown) => void;
  onSettled?: () => void;
};

export interface IssueSurfaceActions {
  isPending: boolean;
  createIssue: (defaults?: IssueCreateDefaults) => void;
  updateIssue: (
    issueId: string,
    updates: Partial<UpdateIssueRequest>,
    options?: IssueSurfaceMutationOptions,
  ) => void;
  moveIssue: (
    issueId: string,
    updates: Partial<UpdateIssueRequest>,
    options?: IssueSurfaceMutationOptions,
  ) => void;
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
  /** Resolves with how many issues the server wrote and which; it skips the ones its per-issue guards refuse. */
||||||| 2ae2f08005
  /** Resolves with how many issues the server wrote; it skips the ones its per-issue guards refuse. */
=======
  /** Resolves with the server's per-issue results; it skips the ones its per-issue guards refuse. */
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
  batchUpdate: (
    issueIds: string[],
    updates: Partial<UpdateIssueRequest>,
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
  ) => Promise<BatchUpdateIssuesResponse>;
||||||| 2ae2f08005
  ) => Promise<{ updated: number }>;
=======
    ops?: { labels?: BatchIssueLabelOps; properties?: BatchIssuePropertyOps },
  ) => Promise<BatchUpdateIssuesResponse>;
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
  batchDelete: (issueIds: string[]) => Promise<void>;
}

const IssueSurfaceActionsContext = createContext<IssueSurfaceActions | null>(
  null,
);

export function IssueSurfaceActionsProvider({
  actions,
  children,
}: {
  actions: IssueSurfaceActions;
  children: ReactNode;
}) {
  return (
    <IssueSurfaceActionsContext.Provider value={actions}>
      {children}
    </IssueSurfaceActionsContext.Provider>
  );
}

export function useIssueSurfaceActionsOptional() {
  return useContext(IssueSurfaceActionsContext);
}
