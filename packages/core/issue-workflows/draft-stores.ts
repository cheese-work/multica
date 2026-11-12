import { createKeyedDraftStore } from "../drafts/create-keyed-draft-store";
import type { IssueWorkflowStep } from "../types";

/** A middle column of a workflow draft: a library status, or one to create on save. */
export type WorkflowDraftColumn =
  | { id: string; status_key: string }
  | { id: string; new_name: string; category?: "unstarted" | "started" };

/** What entering a column does, keyed in the draft by column id. */
export type WorkflowDraftHandoff = Pick<
  IssueWorkflowStep,
  "handler" | "instructions" | "next_status_key" | "back_status_key" | "advance_on_ci_green" | "keep_other_assignee" | "edges"
>;

/** The workflow editor's whole editable state. */
export interface WorkflowEditorDraft {
  name: string;
  description: string;
  backlog: boolean;
  blocked: boolean;
  middle: WorkflowDraftColumn[];
  handoffs: Record<string, WorkflowDraftHandoff>;
  /** Absent in a draft held before the field existed. */
  maxRuns?: string;
  /** Absent in a draft held before the field existed. */
  includeChildPrs?: boolean;
}

/**
 * Unsaved workflow editor state, keyed by editor target (workflow id, or the
 * new/duplicate/template target). `draft` is null until the editor holds
 * edits; `baseVersion` is the saved version they sit on ("" for a new one).
 */
export interface WorkflowEditorDraftEntry {
  draft: WorkflowEditorDraft | null;
  baseVersion: string;
}

export const EMPTY_WORKFLOW_EDITOR_DRAFT: WorkflowEditorDraftEntry = { draft: null, baseVersion: "" };

export const useWorkflowEditorDraftStore = createKeyedDraftStore<WorkflowEditorDraftEntry>({
  storageKey: "multica_workflow_editor_drafts",
  emptyData: EMPTY_WORKFLOW_EDITOR_DRAFT,
  hasMeaningful: (d) => d.draft !== null,
});

/** An unsaved handoff form, keyed by `${workflowId}:${statusKey}`. */
export interface WorkflowHandoffDraft {
  step: IssueWorkflowStep | null;
  scope: "all" | "project";
  /** The saved step the edits sit on, to spot a save made while they were away. */
  base: IssueWorkflowStep | null;
}

export const EMPTY_WORKFLOW_HANDOFF_DRAFT: WorkflowHandoffDraft = { step: null, scope: "all", base: null };

export const useWorkflowHandoffDraftStore = createKeyedDraftStore<WorkflowHandoffDraft>({
  storageKey: "multica_workflow_handoff_drafts",
  emptyData: EMPTY_WORKFLOW_HANDOFF_DRAFT,
  hasMeaningful: (d) => d.step !== null,
});
