/**
 * Draft state of the four form screens (issue edit, project edit, new issue,
 * new project). Each hook owns its screen's field state and saves it through
 * `form-drafts-store`, so an unmount, a modal close or an app restart keeps the
 * unsent input.
 *
 * `restored` turns true once the saved draft has been applied. Until then the
 * screen must not accept input (it would be lost or overwritten by the late
 * restore), and nothing is written. `settle()` clears the draft after a
 * successful save/create or an explicit discard, and stops further writes.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
  IssuePriority,
  IssueStatus,
  Project,
  ProjectPriority,
  ProjectStatus,
} from "@multica/core/types";
import { stripChannelMediaMarkers } from "@multica/core/types";
import type { AssigneeValue } from "@/components/issue/pickers/assignee-picker-body";
import { useMentionInput } from "@/lib/use-mention-input";
import {
  NEW_ISSUE_KEY,
  NEW_PROJECT_KEY,
  issueEditKey,
  projectEditKey,
  readFormDraft,
  useFormDraftsStore,
} from "./form-drafts-store";
import { useNewIssueDraftStore } from "./new-issue-draft-store";
import { useNewProjectDraftStore } from "./new-project-draft-store";

function usePersistedForm<T>(o: {
  key: string;
  /** False until the data the form seeds from (the record) has loaded. */
  enabled: boolean;
  /** Applied on restore when no draft exists (edit screens: server values). */
  seed?: T;
  apply: (values: T) => void;
  value: T;
  /** Nothing to keep: the draft is removed instead of saved. */
  empty: boolean;
}) {
  const ready = useFormDraftsStore((s) => s.ready);
  const restored = useFormDraftsStore((s) => s.restored[o.key] === true);
  const settled = useRef(false);
  const latest = useRef(o);
  useEffect(() => {
    latest.current = o;
  });

  useEffect(() => {
    if (!ready || !o.enabled || restored) return;
    const values = readFormDraft<T>(o.key) ?? latest.current.seed;
    if (values !== undefined) latest.current.apply(values);
    useFormDraftsStore.getState().markRestored(o.key, true);
  }, [ready, o.enabled, restored, o.key]);

  useEffect(
    () => () => useFormDraftsStore.getState().markRestored(o.key, false),
    [o.key],
  );

  useEffect(() => {
    if (!restored || settled.current) return;
    const store = useFormDraftsStore.getState();
    if (o.empty) store.clearDraft(o.key);
    else store.setDraft(o.key, o.value);
  }, [restored, o.empty, o.value, o.key]);

  const key = o.key;
  const settle = useCallback(() => {
    settled.current = true;
    useFormDraftsStore.getState().clearDraft(key);
  }, [key]);

  return { restored, settle };
}

/**
 * What closing a create form must do. Before the saved draft has restored the
 * form looks empty, so closing must leave the draft on disk untouched.
 */
export type CloseMode = "keep" | "discard" | "confirm";

export function closeMode(restored: boolean, dirty: boolean): CloseMode {
  if (!restored) return "keep";
  return dirty ? "confirm" : "discard";
}

export interface IssueRecordText {
  title: string;
  description?: string | null;
}

export function useIssueEditDraft(id: string, server: IssueRecordText | undefined) {
  const [title, setTitle] = useState("");
  const [initial, setInitial] = useState({ title: "", description: "" });
  const description = useMentionInput();
  const currentDescription = description.serialize();
  const setDescriptionText = description.setText;
  const value = useMemo(
    () => ({ title, description: currentDescription }),
    [title, currentDescription],
  );
  const baseline = server && {
    title: server.title,
    description: stripChannelMediaMarkers(server.description ?? ""),
  };
  const dirty =
    title.trim() !== initial.title ||
    currentDescription.trim() !== initial.description.trim();
  const { restored, settle } = usePersistedForm({
    key: issueEditKey(id),
    enabled: !!server,
    seed: baseline,
    apply: (v) => {
      setTitle(v.title);
      setDescriptionText(v.description);
      if (baseline) setInitial(baseline);
    },
    value,
    empty: !dirty,
  });
  return {
    title,
    setTitle,
    description,
    currentDescription,
    restored,
    dirty: restored && dirty,
    settle,
  };
}

export interface ProjectRecordText {
  title: string;
  description?: string | null;
  icon?: string | null;
}

export function useProjectEditDraft(id: string, server: ProjectRecordText | undefined) {
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [icon, setIcon] = useState("");
  const value = useMemo(
    () => ({ title, description, icon }),
    [title, description, icon],
  );
  const dirty =
    !!server &&
    (title.trim() !== server.title ||
      description.trim() !== (server.description ?? "") ||
      icon.trim() !== (server.icon ?? ""));
  const { restored, settle } = usePersistedForm({
    key: projectEditKey(id),
    enabled: !!server,
    seed: server && {
      title: server.title,
      description: server.description ?? "",
      icon: server.icon ?? "",
    },
    apply: (v) => {
      setTitle(v.title);
      setDescription(v.description);
      setIcon(v.icon);
    },
    value,
    empty: !dirty,
  });
  return {
    title,
    setTitle,
    description,
    setDescription,
    icon,
    setIcon,
    restored,
    dirty: restored && dirty,
    settle,
  };
}

interface NewIssueValues {
  title: string;
  description: string;
  status: IssueStatus;
  priority: IssuePriority;
  assignee: AssigneeValue;
  dueDate: string | null;
  project: Project | null;
}

const CLEAN_NEW_ISSUE: NewIssueValues = {
  title: "",
  description: "",
  status: "todo",
  priority: "none",
  assignee: null,
  dueDate: null,
  project: null,
};

export function useNewIssueDraft() {
  const [title, setTitle] = useState("");
  const description = useMentionInput();
  const descriptionText = description.serialize();
  const setDescriptionText = description.setText;
  const status = useNewIssueDraftStore((s) => s.status);
  const priority = useNewIssueDraftStore((s) => s.priority);
  const assignee = useNewIssueDraftStore((s) => s.assignee);
  const dueDate = useNewIssueDraftStore((s) => s.dueDate);
  const project = useNewIssueDraftStore((s) => s.project);
  const value = useMemo<NewIssueValues>(
    () => ({
      title,
      description: descriptionText,
      status,
      priority,
      assignee,
      dueDate,
      project,
    }),
    [title, descriptionText, status, priority, assignee, dueDate, project],
  );
  const { restored, settle } = usePersistedForm<NewIssueValues>({
    key: NEW_ISSUE_KEY,
    enabled: true,
    // No saved draft in this scope: start clean, never keep picker values
    // left in the singleton by another session.
    seed: CLEAN_NEW_ISSUE,
    apply: (v) => {
      setTitle(v.title);
      setDescriptionText(v.description);
      useNewIssueDraftStore.setState({
        status: v.status,
        priority: v.priority,
        assignee: v.assignee,
        dueDate: v.dueDate,
        project: v.project,
      });
    },
    value,
    empty:
      !title &&
      !descriptionText &&
      status === "todo" &&
      priority === "none" &&
      !assignee &&
      !dueDate &&
      !project,
  });
  return { title, setTitle, description, restored, settle };
}

interface NewProjectValues {
  title: string;
  icon: string;
  description: string;
  status: ProjectStatus;
  priority: ProjectPriority;
}

export function useNewProjectDraft() {
  const [title, setTitle] = useState("");
  const [icon, setIcon] = useState("");
  const [description, setDescription] = useState("");
  const status = useNewProjectDraftStore((s) => s.status);
  const priority = useNewProjectDraftStore((s) => s.priority);
  const value = useMemo<NewProjectValues>(
    () => ({ title, icon, description, status, priority }),
    [title, icon, description, status, priority],
  );
  const dirty =
    title.length > 0 ||
    icon.length > 0 ||
    description.length > 0 ||
    status !== "planned" ||
    priority !== "none";
  const { restored, settle } = usePersistedForm<NewProjectValues>({
    key: NEW_PROJECT_KEY,
    enabled: true,
    seed: {
      title: "",
      icon: "",
      description: "",
      status: "planned",
      priority: "none",
    },
    apply: (v) => {
      setTitle(v.title);
      setIcon(v.icon);
      setDescription(v.description);
      useNewProjectDraftStore.setState({
        status: v.status,
        priority: v.priority,
      });
    },
    value,
    empty: !dirty,
  });
  return {
    title,
    setTitle,
    icon,
    setIcon,
    description,
    setDescription,
    restored,
    dirty,
    settle,
    closeMode: closeMode(restored, dirty),
  };
}
