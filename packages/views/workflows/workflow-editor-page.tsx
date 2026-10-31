"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
} from "@dnd-kit/core";
import {
  SortableContext,
  arrayMove,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import {
  ChevronDown,
  ChevronRight,
  Copy,
  CornerDownRight,
  FolderKanban,
  GripVertical,
  MoreHorizontal,
  Plus,
  Trash2,
  UserRound,
} from "@multica/ui/icons";
import { toast } from "sonner";
import { ApiError, clientErrorMessage } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentWorkspace } from "@multica/core/paths";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { statusColumnKeys } from "@multica/core/issues";
import {
  isFixedWorkflowStatus,
  statusAcceptsHandoff,
  useCreateIssueWorkflow,
  useDeleteIssueWorkflow,
  useIssueWorkflows,
  useUpdateIssueWorkflow,
  workflowMiddleKeys,
  MAX_EDGES_PER_STEP,
  validateWorkflowGraph,
  workflowNextStatus,
} from "@multica/core/issue-workflows";
import { useActorName } from "@multica/core/workspace/hooks";
import { projectListOptions } from "@multica/core/projects/queries";
import type {
  IssueWorkflow,
  IssueWorkflowMappingPlan,
  IssueWorkflowStep,
  IssueWorkflowStepInput,
} from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { IconButton } from "@multica/ui/components/common/icon-button";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import { Input } from "@multica/ui/components/ui/input";
import { Spinner } from "@multica/ui/components/ui/spinner";
import { Switch } from "@multica/ui/components/ui/switch";
import { cn } from "@multica/ui/lib/utils";
import { StatusIcon } from "../issues/components/status-icon";
import { useStatusLabel } from "../issues/utils/status-label";
import { ActorAvatar } from "../common/actor-avatar";
import { useT } from "../i18n";
import { BriefPreviewDialog, HandoffFields, savedHandoff } from "./handoff-form";
import { stepHandlerActor, useStepHandlerLabel } from "./step-handler";
import { WORKFLOW_TEMPLATES, type WorkflowTemplateId } from "./templates";
import { WorkflowEdgeEditor } from "./workflow-edge-editor";
import { WorkflowGraph } from "./workflow-graph";
import { WorkflowMappingDialog } from "./workflow-mapping-dialog";

/**
 * The name and description edit in place. A hover or focus background tells
 * them apart from the read-only note under them.
 */
const INLINE_FIELD =
  "-mx-1.5 block w-[calc(100%+0.75rem)] rounded-md bg-transparent px-1.5 outline-none transition-colors placeholder:text-muted-foreground read-only:cursor-default hover:[&:not(:read-only)]:bg-accent/60 focus-visible:[&:not(:read-only)]:bg-accent/60 focus-visible:[&:not(:read-only)]:ring-1 focus-visible:[&:not(:read-only)]:ring-ring/40";

/** A new workflow starts from the common delivery loop instead of nothing. */
const NEW_WORKFLOW_MIDDLE = ["in_progress", "in_review"];

/** A middle column of the draft: a library status, or one to create on save. */
type DraftColumn = { id: string; status_key: string } | { id: string; new_name: string; category?: "unstarted" | "started" };

/** What entering a column does, keyed in the draft by column id. */
type Handoff = Pick<IssueWorkflowStep, "handler" | "instructions" | "next_status_key" | "back_status_key" | "advance_on_ci_green" | "edges">;

const NO_HANDOFF: Handoff = { handler: { type: "none" }, instructions: "" };

interface Draft {
  name: string;
  description: string;
  backlog: boolean;
  blocked: boolean;
  middle: DraftColumn[];
  handoffs: Record<string, Handoff>;
}

/**
 * The key a column goes by in the editor: its status, or — for a status the
 * save creates — its draft id, so its handoff can be edited before the save.
 */
function columnKey(column: DraftColumn): string {
  return "status_key" in column ? column.status_key : column.id;
}

function hasHandoff(h: Handoff | undefined): h is Handoff {
  return !!h && (h.handler.type !== "none" || !!h.next_status_key || !!h.back_status_key || !!h.advance_on_ci_green || !!h.edges?.length);
}

function draftFrom(workflow: Pick<IssueWorkflow, "name" | "description" | "steps">): Draft {
  const keys = workflow.steps.map((s) => s.status_key);
  const handoffs: Record<string, Handoff> = {};
  for (const s of workflow.steps) {
    const h = {
      handler: s.handler,
      instructions: s.instructions,
      next_status_key: s.next_status_key,
      back_status_key: s.back_status_key,
      advance_on_ci_green: s.advance_on_ci_green,
      edges: s.edges,
    };
    if (hasHandoff(h)) handoffs[s.status_key] = h;
  }
  return {
    name: workflow.name,
    description: workflow.description,
    backlog: keys.includes("backlog"),
    blocked: keys.includes("blocked"),
    middle: workflowMiddleKeys(workflow).map((key) => ({ id: key, status_key: key })),
    handoffs,
  };
}

/** Every column of the draft in board order. */
function draftColumns(draft: Draft): DraftColumn[] {
  const fixed = (key: string): DraftColumn => ({ id: key, status_key: key });
  return [
    ...(draft.backlog ? [fixed("backlog")] : []),
    fixed("todo"),
    ...draft.middle,
    ...(draft.blocked ? [fixed("blocked")] : []),
    fixed("done"),
    fixed("cancelled"),
  ];
}

function sameDraft(a: Draft, b: Draft): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

function isChangedElsewhere(err: unknown): boolean {
  if (!(err instanceof ApiError) || err.status !== 409 || !err.body || typeof err.body !== "object") return false;
  return (err.body as { code?: unknown }).code === "workflow_changed_elsewhere";
}

function mappingPlanFrom(err: unknown): IssueWorkflowMappingPlan | null {
  if (!(err instanceof ApiError) || err.status !== 409 || !err.body || typeof err.body !== "object") return null;
  const body = err.body as { code?: unknown; plan?: IssueWorkflowMappingPlan };
  if (body.code !== "workflow_status_mapping_required" || !body.plan) return null;
  return { ...body.plan, unchanged: body.plan.unchanged ?? [] };
}

/**
 * The workflow editor (MUL-7420): a workflow is a whole pipeline — its
 * columns and who each one hands an issue to. Todo, done and cancelled are
 * always there; backlog and blocked can be switched off; the editor arranges
 * what sits between. Selecting a column edits its handoff.
 *
 * `workflowId` is null for a new workflow; `seed` pre-fills one (duplicate).
 */
export function WorkflowEditorPage({
  workflowId,
  seed,
  template,
  canEdit,
  canDuplicate = canEdit,
  canCreateStatus = canEdit,
  onBack,
  onOpen,
}: {
  workflowId: string | null;
  seed: IssueWorkflow | null;
  /** Pre-fills a new workflow from a template. */
  template?: WorkflowTemplateId | null;
  canEdit: boolean;
  /** Duplicating creates a workflow, which any member may do. */
  canDuplicate?: boolean;
  /**
   * Whether saving may add statuses to the workspace catalog: statuses are
   * shared by every board, so only admins create them (MUL-8120).
   */
  canCreateStatus?: boolean;
  onBack: () => void;
  /** Navigates to another workflow's editor (after create, or to a duplicate). */
  onOpen: (target: { id: string } | { duplicateOf: string }) => void;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const catalog = useIssueStatuses(wsId);
  const labelOf = useStatusLabel(wsId);
  const { workflows, isLoaded, isError: loadFailed, retry: retryLoad } = useIssueWorkflows(wsId);
  const { data: projects = [] } = useQuery(projectListOptions(wsId));
  const createWorkflow = useCreateIssueWorkflow();
  const updateWorkflow = useUpdateIssueWorkflow();
  const deleteWorkflow = useDeleteIssueWorkflow();

  const saved = workflowId ? workflows.find((w) => w.id === workflowId) ?? null : null;
  const isDefault = !!saved?.is_default;
  const baseline = useMemo<Draft>(() => {
    if (saved) return draftFrom(saved);
    if (seed) return { ...draftFrom(seed), name: t(($) => $.workflows.editor.copy_name, { name: seed.name }) };
    const tpl = template ? WORKFLOW_TEMPLATES.find((x) => x.id === template) : undefined;
    if (tpl) {
      const statusName = (name: string) =>
        t(($) => ($.workflows.templates[tpl.id].statuses as Record<string, string>)[name] ?? name);
      return {
        name: t(($) => $.workflows.templates[tpl.id].name),
        description: t(($) => $.workflows.templates[tpl.id].summary),
        backlog: true,
        blocked: true,
        handoffs: {},
        middle: tpl.columns.map((column): DraftColumn => {
          if ("status" in column) return { id: column.status, status_key: column.status };
          const name = statusName(column.name);
          const existing = catalog.activeStatuses.find((e) => e.name.toLowerCase() === name.toLowerCase());
          return existing
            ? { id: existing.key, status_key: existing.key }
            : { id: `new:${name}`, new_name: name, category: column.category };
        }),
      };
    }
    return {
      name: "",
      description: "",
      backlog: true,
      blocked: true,
      middle: NEW_WORKFLOW_MIDDLE.map((key) => ({ id: key, status_key: key })),
      handoffs: {},
    };
    // Re-seed when the target changes or the saved row is refreshed. The version
    // is in the key because updated_at has one-second resolution: two saves in a
    // second share it and differ only in version.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [saved?.version, saved?.updated_at, workflowId, seed?.id, template, catalog.activeStatuses.length]);

  const [draft, setDraft] = useState<Draft>(baseline);
  const [mappingPlan, setMappingPlan] = useState<IssueWorkflowMappingPlan | null>(null);
  const [newName, setNewName] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [previewOpen, setPreviewOpen] = useState(false);
  // The saved workflow refreshes under the editor: updated_at moves, the status
  // catalog grows, another admin saves. A clean editor follows it. An editor with
  // unsaved edits keeps them. When the refresh changed the workflow's content,
  // saving would overwrite that change, so the editor says so and holds Save until
  // the editor picks a version.
  const [changedElsewhere, setChangedElsewhere] = useState(false);
  // Read by the refresh effect and by every save entrypoint, which must see the
  // conflict the moment it is set rather than on the next render.
  const conflictRef = useRef(false);
  conflictRef.current = changedElsewhere;
  const draftRef = useRef(draft);
  draftRef.current = draft;
  const targetKey = `${workflowId ?? ""}|${seed?.id ?? ""}|${template ?? ""}`;
  // The version of the saved workflow that the baseline, and so the draft's
  // unedited parts, came from. A save names this one and no other, so the token
  // and the steps it travels with always belong to the same accepted revision.
  const acceptedVersion = useRef<string | undefined>(saved?.version || undefined);
  // The version this editor's own last successful save produced. Its refresh is
  // not another admin's change, so it raises no alert.
  const ownSavedVersion = useRef<string | undefined>(undefined);
  const latestVersion = saved?.version || undefined;
  const synced = useRef({ targetKey, baseline });
  useEffect(() => {
    const previous = synced.current;
    synced.current = { targetKey, baseline };
    if (previous.targetKey !== targetKey) {
      setDraft(baseline);
      setChangedElsewhere(false);
      acceptedVersion.current = latestVersion;
      ownSavedVersion.current = undefined;
      return;
    }
    if (previous.baseline === baseline) return;
    const current = draftRef.current;
    if (sameDraft(current, previous.baseline) || sameDraft(current, baseline)) {
      setDraft(baseline);
      setChangedElsewhere(false);
      acceptedVersion.current = latestVersion;
      return;
    }
    if (workflowId && latestVersion && latestVersion === ownSavedVersion.current) {
      // This editor's own save arrived and the draft carries edits made after
      // it was sent: they sit on the saved content.
      acceptedVersion.current = latestVersion;
      return;
    }
    if (workflowId && !sameDraft(previous.baseline, baseline)) {
      setChangedElsewhere(true);
      return;
    }
    // The refresh changed nothing the editor shows, so the edits sit on the same
    // content as the new version: follow its token. While a conflict is open the
    // content the edits sit on is the version the editor started from, not this
    // one; only the explicit choices move the token then.
    if (!conflictRef.current) acceptedVersion.current = latestVersion;
  }, [baseline, targetKey, workflowId, latestVersion]);

  const dirty = !workflowId || !sameDraft(draft, baseline);
  const pending = createWorkflow.isPending || updateWorkflow.isPending;
  // Statuses this save would create; a member must drop them or ask an admin.
  const missingStatuses = canCreateStatus
    ? []
    : draft.middle.flatMap((c) => ("new_name" in c ? [c.new_name] : []));
  // An existing workflow is edited and saved only from its loaded row.
  const rowReady = !workflowId || !!saved;
  // The graph and exits form edit steps that exist before the save; a column
  // the save creates has no status key to point an exit at.
  const graphSteps = useMemo<IssueWorkflowStep[]>(
    () => draftColumns(draft).flatMap((c) => ("status_key" in c ? [{ status_key: c.status_key, ...(draft.handoffs[c.id] ?? NO_HANDOFF) }] : [])),
    [draft],
  );
  const graphIssues = useMemo(() => validateWorkflowGraph(graphSteps), [graphSteps]);
  const canSave = canEdit && rowReady && dirty && !changedElsewhere && draft.name.trim().length > 0 && !pending && missingStatuses.length === 0 && graphIssues.length === 0;
  const usedBy = saved
    ? projects.filter((p) => (saved.is_default ? !p.workflow_id : saved.project_ids.includes(p.id)))
    : [];
  const columns = draftColumns(draft);
  const middleKeys = draft.middle.flatMap((c) => ("status_key" in c ? [c.status_key] : []));
  const addable = statusColumnKeys(catalog).filter((key) => !isFixedWorkflowStatus(key) && !middleKeys.includes(key));
  const ids = draft.middle.map((c) => c.id);
  const targetKeys = columns.flatMap((c) => ("status_key" in c ? [c.status_key] : []));
  // Next and back can only name statuses that exist before the save.
  const flow = { steps: targetKeys.map((status_key) => ({ status_key, ...NO_HANDOFF })) };
  // The default next follows the whole draft, unsaved columns included —
  // the order the server resolves it in once the draft is saved.
  const draftFlow = { steps: columns.map((c) => ({ status_key: columnKey(c), ...NO_HANDOFF })) };
  const columnLabel = (key: string) => {
    if (!key) return "";
    const column = draft.middle.find((c) => c.id === key && "new_name" in c);
    return column && "new_name" in column ? column.new_name : labelOf(key);
  };
  const selectedId = selected ?? "todo";
  const selectedColumn = columns.find((c) => c.id === selectedId && "status_key" in c && statusAcceptsHandoff(c.status_key));
  const selectedStep: IssueWorkflowStep | null =
    selectedColumn && "status_key" in selectedColumn
      ? { status_key: selectedColumn.status_key, ...(draft.handoffs[selectedColumn.id] ?? NO_HANDOFF) }
      : null;
  const patchHandoff = (id: string, patch: Partial<Handoff>) =>
    setDraft((d) => ({ ...d, handoffs: { ...d.handoffs, [id]: { ...(d.handoffs[id] ?? NO_HANDOFF), ...patch } } }));
  const onGraphChange = (next: IssueWorkflowStep[]) =>
    setDraft((d) => {
      const before = new Map(graphSteps.map((s) => [s.status_key, s]));
      const handoffs = { ...d.handoffs };
      for (const s of next) {
        if (s === before.get(s.status_key)) continue;
        const { handler, instructions, next_status_key, back_status_key, advance_on_ci_green, edges } = s;
        handoffs[s.status_key] = { handler, instructions, next_status_key, back_status_key, advance_on_ci_green, edges };
      }
      return { ...d, handoffs };
    });

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );
  const onDragEnd = ({ active, over }: DragEndEvent) => {
    if (!over || active.id === over.id) return;
    setDraft((d) => ({ ...d, middle: arrayMove(d.middle, ids.indexOf(String(active.id)), ids.indexOf(String(over.id))) }));
  };

  const addNew = () => {
    const name = newName?.trim();
    if (!name) return;
    const id = `new:${name}:${Date.now()}`;
    setDraft((d) => ({ ...d, middle: [...d.middle, { id, new_name: name }] }));
    setSelected(id);
    setNewName(null);
  };

  const save = async (mapping?: Record<string, string>) => {
    // Never write an existing workflow from defaults the row has not replaced.
    if (workflowId && !saved) return;
    // An unresolved conflict holds every save, including the mapping dialog's:
    // the draft sits on a version another admin has since replaced.
    if (workflowId && conflictRef.current) {
      toast.error(t(($) => $.workflows.editor.changed_elsewhere));
      return;
    }
    const listed = new Set(targetKeys);
    const steps: IssueWorkflowStepInput[] = columns.map((c) => {
      const raw = draft.handoffs[c.id];
      const h = raw && savedHandoff(raw);
      const handoff = hasHandoff(h)
        ? {
            ...(h.handler.type !== "none" ? { handler: h.handler, instructions: h.instructions } : {}),
            ...(h.next_status_key && listed.has(h.next_status_key) ? { next_status_key: h.next_status_key } : {}),
            ...(h.back_status_key && listed.has(h.back_status_key) ? { back_status_key: h.back_status_key } : {}),
            ...(h.advance_on_ci_green ? { advance_on_ci_green: true } : {}),
            ...(h.edges?.length ? { edges: h.edges } : {}),
          }
        : {};
      return "status_key" in c
        ? { status_key: c.status_key, ...handoff }
        : { new_status: { name: c.new_name, ...(c.category ? { category: c.category } : {}) }, ...handoff };
    });
    const body = {
      ...(isDefault ? {} : { name: draft.name.trim() }),
      description: draft.description.trim(),
      steps,
      ...(mapping ? { status_mapping: mapping } : {}),
      // Save against the version the editor started from, so a save that
      // arrives after another admin's cannot replace it.
      ...(workflowId && acceptedVersion.current ? { expected_version: acceptedVersion.current } : {}),
    };
    try {
      if (workflowId) {
        const submitted = draft;
        const startedOn = targetKey;
        const response = await updateWorkflow.mutateAsync({ id: workflowId, ...body });
        toast.success(t(($) => $.workflows.editor.saved));
        // A late response for another target changes nothing here.
        if (synced.current.targetKey === startedOn) {
          if (response?.version) {
            ownSavedVersion.current = response.version;
            acceptedVersion.current = response.version;
          }
          // Adopt the saved revision only when nothing was typed since the save
          // began. Edits made while it was in flight stay, on top of the revision
          // the server accepted.
          if (response?.steps && sameDraft(draftRef.current, submitted)) {
            setDraft(draftFrom(response));
            setChangedElsewhere(false);
          }
        }
      } else {
        const created = await createWorkflow.mutateAsync({ ...body, name: draft.name.trim() });
        toast.success(t(($) => $.workflows.editor.created));
        onOpen({ id: created.id });
      }
      setMappingPlan(null);
    } catch (err) {
      const plan = mappingPlanFrom(err);
      if (plan) {
        setMappingPlan(plan);
        return;
      }
      if (workflowId && isChangedElsewhere(err)) {
        // The mutation refreshes the workflow list. The edits stay, and the
        // editor chooses between them and the other admin's version.
        setMappingPlan(null);
        setChangedElsewhere(true);
        return;
      }
      toast.error(clientErrorMessage(err) ?? t(($) => $.workflows.editor.error));
    }
  };

  const remove = async () => {
    if (!workflowId) return;
    try {
      await deleteWorkflow.mutateAsync(workflowId);
      toast.success(t(($) => $.workflows.settings.deleted));
      onBack();
    } catch (err) {
      toast.error(clientErrorMessage(err) ?? t(($) => $.workflows.settings.delete_error));
    }
  };

  if (workflowId && !saved && !isLoaded) {
    return (
      <div className="space-y-4" aria-busy={!loadFailed}>
        <Breadcrumb onBack={onBack} current="" />
        {loadFailed ? (
          <div role="alert" className="flex flex-wrap items-center gap-3 text-body text-muted-foreground">
            <span>{t(($) => $.workflows.editor.load_error)}</span>
            <Button variant="outline" size="sm" onClick={retryLoad}>
              {t(($) => $.workflows.editor.retry)}
            </Button>
          </div>
        ) : (
          <Spinner className="size-4" />
        )}
      </div>
    );
  }

  if (workflowId && isLoaded && !saved) {
    return (
      <div className="space-y-4">
        <Breadcrumb onBack={onBack} current="" />
        <p className="text-body text-muted-foreground">{t(($) => $.workflows.editor.not_found)}</p>
      </div>
    );
  }

  const title = draft.name.trim() || t(($) => $.workflows.editor.create_title);
  const middleIds = new Set(ids);
  const isOff = (column: DraftColumn) =>
    "status_key" in column &&
    ((column.status_key === "backlog" && !draft.backlog) || (column.status_key === "blocked" && !draft.blocked));
  const listed: DraftColumn[] = [
    { id: "backlog", status_key: "backlog" },
    { id: "todo", status_key: "todo" },
    ...draft.middle,
    { id: "blocked", status_key: "blocked" },
    { id: "done", status_key: "done" },
    { id: "cancelled", status_key: "cancelled" },
  ];
  const current = listed.find((c) => c.id === selectedId) ?? listed[1]!;
  const nextOf = (column: DraftColumn, h: Handoff | undefined) =>
    h?.next_status_key ?? workflowNextStatus(draftFlow, columnKey(column));
  const card = (column: DraftColumn) => {
    const key = "status_key" in column ? column.status_key : null;
    const optional = key === "backlog" || key === "blocked";
    const off = isOff(column);
    const props = {
      column,
      off,
      start: key === "todo",
      handoff: off ? undefined : draft.handoffs[column.id],
      nextLabel: off ? undefined : columnLabel(nextOf(column, draft.handoffs[column.id])) || undefined,
      selected: current.id === column.id,
      onSelect: () => setSelected(column.id),
      toggle: optional
        ? { checked: !off, disabled: !canEdit, onChange: (on: boolean) => setDraft((d) => ({ ...d, [key]: on })) }
        : undefined,
    };
    return (
      <li key={column.id}>
        {middleIds.has(column.id) ? <SortableStepCard {...props} draggable={canEdit} /> : <StepCard {...props} />}
      </li>
    );
  };
  const replaceStatus = (column: DraftColumn, key: string) => {
    setDraft((d) => {
      const { [column.id]: moved, ...rest } = d.handoffs;
      return {
        ...d,
        middle: d.middle.map((c) => (c.id === column.id ? { id: key, status_key: key } : c)),
        handoffs: moved ? { ...rest, [key]: moved } : rest,
      };
    });
    setSelected(key);
  };
  const removeColumn = (column: DraftColumn) => {
    // Exits into the removed column go with it, legacy next/back included, and
    // a timeout that fell back to it is left for the form to flag.
    const key = columnKey(column);
    setDraft((d) => ({
      ...d,
      middle: d.middle.filter((c) => c.id !== column.id),
      handoffs: Object.fromEntries(
        Object.entries(d.handoffs).map(([id, h]) => [
          id,
          {
            ...h,
            next_status_key: h.next_status_key === key ? undefined : h.next_status_key,
            back_status_key: h.back_status_key === key ? undefined : h.back_status_key,
            ...(h.edges && { edges: h.edges.filter((e) => e.to !== key).map((e) => (e.on_timeout === key ? { ...e, on_timeout: undefined } : e)) }),
          },
        ]),
      ),
    }));
    setSelected("todo");
  };

  return (
    <div className="space-y-6">
      <div className="space-y-3">
        <Breadcrumb onBack={onBack} current={title} />
        <div className="flex items-start gap-3">
          <div className="min-w-0 flex-1">
            <input
              aria-label={t(($) => $.workflows.editor.name)}
              value={draft.name}
              maxLength={64}
              readOnly={!canEdit || isDefault}
              placeholder={t(($) => $.workflows.editor.name_placeholder)}
              onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
              className={cn(INLINE_FIELD, "h-9 text-title-lg font-semibold tracking-tight")}
            />
            <input
              aria-label={t(($) => $.workflows.editor.description)}
              value={draft.description}
              maxLength={256}
              readOnly={!canEdit}
              placeholder={canEdit ? t(($) => $.workflows.editor.description_placeholder) : ""}
              onChange={(e) => setDraft((d) => ({ ...d, description: e.target.value }))}
              className={cn(INLINE_FIELD, "mt-0.5 h-7 text-body")}
            />
            <p className="mt-2 flex items-start gap-1.5 text-caption leading-5 text-muted-foreground">
              <FolderKanban aria-hidden className="mt-0.5 size-3.5 shrink-0" />
              <span>
                {isDefault
                  ? t(($) => $.workflows.editor.default_note)
                  : usedBy.length > 0
                    ? t(($) => $.workflows.editor.used_by_note, {
                        count: usedBy.length,
                        names: usedBy.map((p) => p.title).join(t(($) => $.workflows.list_separator)),
                      })
                    : t(($) => $.workflows.editor.unused_note)}
              </span>
            </p>
          </div>
          {(canEdit || canDuplicate) && (
            <div className="flex shrink-0 items-center gap-2">
              {canEdit && dirty && workflowId && (
                <span className="text-caption text-muted-foreground">{t(($) => $.workflows.editor.unsaved)}</span>
              )}
              {canDuplicate && workflowId && (
                <Button variant="outline" className="gap-1.5" onClick={() => onOpen({ duplicateOf: workflowId })}>
                  <Copy className="size-3.5" />
                  {t(($) => $.workflows.editor.duplicate)}
                </Button>
              )}
              {canEdit && (
                <Button onClick={() => void save()} disabled={!canSave} aria-busy={pending}>
                  {pending && <Spinner className="size-3.5" />}
                  {workflowId ? t(($) => $.workflows.editor.save) : t(($) => $.workflows.editor.create)}
                </Button>
              )}
              {canEdit && workflowId && !isDefault && (
                <DropdownMenu>
                  <DropdownMenuTrigger
                    render={
                      <IconButton variant="ghost" size="icon" label={t(($) => $.workflows.editor.more)} tooltipSide="bottom">
                        <MoreHorizontal className="size-4" />
                      </IconButton>
                    }
                  />
                  <DropdownMenuContent align="end" className="w-56">
                    <DropdownMenuItem
                      variant="destructive"
                      disabled={usedBy.length > 0 || deleteWorkflow.isPending}
                      onClick={() => void remove()}
                    >
                      <Trash2 className="size-3.5" />
                      {t(($) => $.workflows.settings.delete)}
                    </DropdownMenuItem>
                    {usedBy.length > 0 && (
                      <p className="px-2 pb-1.5 text-caption text-muted-foreground">
                        {t(($) => $.workflows.settings.delete_in_use)}
                      </p>
                    )}
                  </DropdownMenuContent>
                </DropdownMenu>
              )}
            </div>
          )}
        </div>
        {changedElsewhere && canEdit && (
          <div
            role="alert"
            className="flex flex-wrap items-center gap-3 rounded-md border bg-muted/50 px-3 py-2 text-body"
          >
            <span className="min-w-0 flex-1">{t(($) => $.workflows.editor.changed_elsewhere)}</span>
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                setDraft(baseline);
                acceptedVersion.current = latestVersion;
                // A mapping plan was computed for the draft being dropped.
                setMappingPlan(null);
                setChangedElsewhere(false);
              }}
            >
              {t(($) => $.workflows.editor.load_latest)}
            </Button>
            <Button
              size="sm"
              onClick={() => {
                // Explicit choice: these edits replace the version now showing.
                acceptedVersion.current = latestVersion;
                setChangedElsewhere(false);
              }}
            >
              {t(($) => $.workflows.editor.keep_edits)}
            </Button>
          </div>
        )}
      </div>

      <WorkflowGraph
        workflow={{ steps: graphSteps }}
        project={null}
        currentStatus={selectedId}
        onChange={canEdit ? onGraphChange : undefined}
        onSelectStep={setSelected}
      />
      <WorkflowEdgeEditor steps={graphSteps} stepKey={graphSteps.some((s) => s.status_key === selectedId) ? selectedId : null} canEdit={canEdit} onChange={onGraphChange} />
      {graphIssues.length > 0 && (
        <ul role="alert" className="space-y-1 text-caption text-destructive">
          {graphIssues.map((i) => (
            <li key={`${i.code}:${i.from}:${i.index ?? ""}`}>
              {t(($) => $.workflows.flow.edit.issues[i.code], { from: labelOf(i.from), max: MAX_EDGES_PER_STEP })}
            </li>
          ))}
        </ul>
      )}

      <div className="grid items-start gap-5 lg:grid-cols-[minmax(0,20rem)_minmax(0,1fr)]">
        <div className="min-w-0">
          <ol className="space-y-2">
            {card(listed[0]!)}
            {card(listed[1]!)}
            <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onDragEnd}>
              <SortableContext items={ids} strategy={verticalListSortingStrategy}>
                {draft.middle.map(card)}
              </SortableContext>
            </DndContext>
            {listed.slice(-3).map(card)}
          </ol>
          {canEdit && (
            <div className="mt-3 flex flex-wrap items-center gap-2">
              <DropdownMenu>
                <DropdownMenuTrigger
                  render={
                    <Button variant="outline" size="sm" className="gap-1.5">
                      <Plus className="size-3.5" />
                      {t(($) => $.workflows.editor.add_step)}
                    </Button>
                  }
                />
                <DropdownMenuContent align="start" className="max-h-80 w-56 overflow-y-auto">
                  {addable.map((key) => (
                    <DropdownMenuItem
                      key={key}
                      onClick={() => {
                        setDraft((d) => ({ ...d, middle: [...d.middle, { id: key, status_key: key }] }));
                        setSelected(key);
                      }}
                    >
                      <StatusIcon
                        status={key}
                        category={catalog.categoryOf(key)}
                        color={catalog.colorOf(key)}
                        icon={catalog.iconOf(key)}
                        className="h-3.5 w-3.5"
                      />
                      <span className="truncate">{labelOf(key)}</span>
                    </DropdownMenuItem>
                  ))}
                  {canCreateStatus && (
                    <>
                      {addable.length > 0 && <DropdownMenuSeparator />}
                      <DropdownMenuItem onClick={() => setNewName("")}>
                        <Plus className="size-3.5" />
                        {t(($) => $.workflows.columns.new_status)}
                      </DropdownMenuItem>
                    </>
                  )}
                </DropdownMenuContent>
              </DropdownMenu>
              {newName !== null && (
                <div className="flex items-center gap-1.5">
                  <Input
                    autoFocus
                    maxLength={64}
                    value={newName}
                    className="h-8 w-44"
                    placeholder={t(($) => $.workflows.columns.name_placeholder)}
                    onChange={(e) => setNewName(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") addNew();
                      if (e.key === "Escape") setNewName(null);
                    }}
                  />
                  <Button size="sm" variant="ghost" onClick={addNew} disabled={!newName.trim()}>
                    {t(($) => $.workflows.columns.add)}
                  </Button>
                </div>
              )}
            </div>
          )}
          {canEdit && missingStatuses.length > 0 && (
            <p role="alert" className="mt-2.5 text-caption leading-5 text-destructive">
              {t(($) => $.workflows.editor.status_needs_admin, {
                names: missingStatuses.join(t(($) => $.workflows.list_separator)),
              })}
            </p>
          )}
          <p className="mt-2.5 text-caption leading-5 text-muted-foreground">{t(($) => $.workflows.editor.steps_hint)}</p>
        </div>

        <StepDetail
          key={current.id}
          column={current}
          off={isOff(current)}
          handoff={draft.handoffs[current.id]}
          flow={flow}
          defaultNextLabel={columnLabel(workflowNextStatus(draftFlow, columnKey(current)))}
          canEdit={canEdit}
          statusOptions={middleIds.has(current.id) ? addable : null}
          onReplaceStatus={(key) => replaceStatus(current, key)}
          onRemove={canEdit && middleIds.has(current.id) ? () => removeColumn(current) : undefined}
          onPatch={(patch) => patchHandoff(current.id, patch)}
          onPreview={
            saved && "status_key" in current && saved.steps.some((s) => s.status_key === current.status_key)
              ? () => setPreviewOpen(true)
              : undefined
          }
        />
        {saved && selectedStep && (
          <BriefPreviewDialog open={previewOpen} onOpenChange={setPreviewOpen} workflowId={saved.id} step={selectedStep} />
        )}
      </div>

      <WorkflowMappingDialog
        open={mappingPlan !== null}
        onOpenChange={(open) => !open && setMappingPlan(null)}
        variant={{ kind: "edit", workflowName: draft.name.trim() }}
        plan={mappingPlan}
        targetKeys={targetKeys}
        pending={pending}
        onConfirm={(mapping) => void save(mapping)}
      />
    </div>
  );
}

function Breadcrumb({ onBack, current }: { onBack: () => void; current: string }) {
  const { t } = useT("issues");
  const workspace = useCurrentWorkspace();
  return (
    <nav className="flex items-center gap-1.5 text-caption text-muted-foreground">
      {workspace?.name && (
        <>
          <span className="truncate">{workspace.name}</span>
          <ChevronRight aria-hidden className="size-3" />
        </>
      )}
      <button type="button" onClick={onBack} className="hover:text-foreground transition-colors">
        {t(($) => $.workflows.settings.title)}
      </button>
      {current && (
        <>
          <ChevronRight aria-hidden className="size-3" />
          <span className="truncate text-foreground">{current}</span>
        </>
      )}
    </nav>
  );
}

interface StepCardProps {
  column: DraftColumn;
  /** Backlog or blocked switched off: the workflow leaves the status out. */
  off: boolean;
  /** The step new issues start on. */
  start: boolean;
  handoff: Handoff | undefined;
  /** Where the handler is told to move the issue next, by label. */
  nextLabel: string | undefined;
  selected: boolean;
  onSelect: () => void;
  toggle?: { checked: boolean; disabled: boolean; onChange: (on: boolean) => void };
}

function SortableStepCard({ draggable, ...props }: StepCardProps & { draggable: boolean }) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: props.column.id,
    disabled: !draggable,
  });
  return (
    <StepCard
      {...props}
      sortable={{
        ref: setNodeRef,
        style: { transform: CSS.Transform.toString(transform), transition },
        dragging: isDragging,
        handle: draggable ? { ...attributes, ...listeners } : undefined,
      }}
    />
  );
}

/**
 * One step of the editor's list: its status and what entering it does.
 * Middle steps drag; backlog and blocked switch on and off.
 */
function StepCard({
  column,
  off,
  start,
  handoff,
  nextLabel,
  selected,
  onSelect,
  toggle,
  sortable,
}: StepCardProps & {
  sortable?: {
    ref: (node: HTMLElement | null) => void;
    style: React.CSSProperties;
    dragging: boolean;
    handle?: React.HTMLAttributes<HTMLButtonElement>;
  };
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const catalog = useIssueStatuses(wsId);
  const labelOf = useStatusLabel(wsId);
  const handlerLabel = useStepHandlerLabel(null);
  const { getActorName } = useActorName();
  const label = "status_key" in column ? labelOf(column.status_key) : column.new_name;
  const step = handoff ? { status_key: columnKey(column), ...handoff } : undefined;
  const who = handlerLabel(step);
  const actor = stepHandlerActor(step, null);

  return (
    <div
      ref={sortable?.ref}
      style={sortable?.style}
      className={cn(
        "flex items-start gap-1 rounded-lg border px-2 py-2.5 transition-colors",
        selected ? "border-foreground/20 bg-accent" : "border-surface-border bg-card hover:bg-accent/50",
        off && !selected && "border-dashed bg-transparent",
        sortable?.dragging && "z-10 shadow-[var(--surface-shadow)]",
      )}
    >
      {sortable?.handle ? (
        <TooltipHint label={t(($) => $.workflows.editor.reorder, { name: label })}>
          <button
            type="button"
            aria-label={t(($) => $.workflows.editor.reorder, { name: label })}
            className="flex h-5 w-4 shrink-0 touch-none items-center justify-center rounded-xs cursor-grab text-muted-foreground active:cursor-grabbing"
            {...sortable.handle}
          >
            <GripVertical className="size-3.5" />
          </button>
        </TooltipHint>
      ) : (
        <span className="h-5 w-4 shrink-0" />
      )}
      <button
        type="button"
        onClick={onSelect}
        aria-pressed={selected}
        className={cn("min-w-0 flex-1 text-left", off && "opacity-60")}
      >
        <span className="flex h-5 items-center gap-2">
          {"status_key" in column ? (
            <StatusIcon
              status={column.status_key}
              category={catalog.categoryOf(column.status_key)}
              color={catalog.colorOf(column.status_key)}
              icon={catalog.iconOf(column.status_key)}
              className="h-3.5 w-3.5 shrink-0"
            />
          ) : (
            <Plus aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />
          )}
          <span className="min-w-0 truncate text-body font-medium">{label}</span>
          {start && <Badge>{t(($) => $.workflows.editor.start)}</Badge>}
          {!("status_key" in column) && <Badge>{t(($) => $.workflows.editor.new_badge)}</Badge>}
        </span>
        <span className="mt-1 flex min-w-0 items-center gap-1 text-caption text-muted-foreground">
          {off ? (
            <span className="truncate">{t(($) => $.workflows.editor.step_off_short)}</span>
          ) : who ? (
            <>
              <CornerDownRight aria-hidden className="size-3 shrink-0" />
              <span className="shrink-0">{t(($) => $.workflows.editor.card_hand_to)}</span>
              {actor && <ActorAvatar actorType={actor.type} actorId={actor.id} size="xs" profileLink={false} />}
              <span className="min-w-0 truncate text-foreground">{actor ? getActorName(actor.type, actor.id) : who}</span>
              {handoff?.handler.type === "agent" && nextLabel && (
                <span className="shrink-0 truncate">· {t(($) => $.workflows.editor.card_then, { status: nextLabel })}</span>
              )}
            </>
          ) : (
            <>
              <UserRound aria-hidden className="size-3 shrink-0" />
              <span className="truncate">{t(($) => $.workflows.handler.none)}</span>
            </>
          )}
        </span>
      </button>
      {toggle && (
        <Switch
          size="sm"
          className="mt-0.5"
          checked={toggle.checked}
          disabled={toggle.disabled}
          onCheckedChange={toggle.onChange}
          aria-label={t(($) => $.workflows.editor.include_status, { status: label })}
        />
      )}
    </div>
  );
}

function Badge({ children }: { children: React.ReactNode }) {
  return (
    <span className="shrink-0 rounded-xs bg-muted px-1.5 py-0.5 text-micro text-muted-foreground">{children}</span>
  );
}

/** The selected step: its status and its handoff. */
function StepDetail({
  column,
  off,
  handoff,
  flow,
  canEdit,
  statusOptions,
  onReplaceStatus,
  onRemove,
  onPatch,
  onPreview,
  defaultNextLabel,
}: {
  column: DraftColumn;
  off: boolean;
  handoff: Handoff | undefined;
  flow: Pick<IssueWorkflow, "steps">;
  canEdit: boolean;
  /** Statuses a middle step can switch to; null for fixed steps. */
  statusOptions: string[] | null;
  onReplaceStatus: (key: string) => void;
  onRemove?: () => void;
  onPatch: (patch: Partial<Handoff>) => void;
  onPreview?: () => void;
  /** The default next status's label, resolved over the whole draft. */
  defaultNextLabel: string;
}) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const catalog = useIssueStatuses(wsId);
  const labelOf = useStatusLabel(wsId);
  const key = "status_key" in column ? column.status_key : null;
  const label = key ? labelOf(key) : "new_name" in column ? column.new_name : "";
  const icon = (statusKey: string, className = "h-3.5 w-3.5 shrink-0") => (
    <StatusIcon
      status={statusKey}
      category={catalog.categoryOf(statusKey)}
      color={catalog.colorOf(statusKey)}
      icon={catalog.iconOf(statusKey)}
      className={className}
    />
  );
  const statusField = (
    <span className="flex min-w-0 items-center gap-2">
      {key ? icon(key) : <Plus aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />}
      <span className="truncate">{label}</span>
    </span>
  );

  return (
    <section className="min-w-0 rounded-xl border border-surface-border bg-card lg:sticky lg:top-4">
      <header className="flex items-center gap-2 border-b border-surface-border px-5 py-3.5">
        {key ? icon(key, "h-4 w-4 shrink-0") : <Plus aria-hidden className="size-4 shrink-0 text-muted-foreground" />}
        <h3 className="min-w-0 truncate text-body font-semibold">{label}</h3>
        {key ? (
          <code className="truncate font-mono text-caption text-muted-foreground">{key}</code>
        ) : (
          <Badge>{t(($) => $.workflows.editor.new_badge)}</Badge>
        )}
        {onRemove && (
          <IconButton
            variant="ghost"
            size="icon-sm"
            className="ml-auto text-muted-foreground"
            label={t(($) => $.workflows.editor.remove)}
            onClick={onRemove}
          >
            <Trash2 className="size-3.5" />
          </IconButton>
        )}
      </header>
      <div className="space-y-6 p-5">
        <DetailField label={t(($) => $.workflows.editor.status)} hint={key ? t(($) => $.workflows.editor.status_hint) : t(($) => $.workflows.editor.status_new_hint)}>
          {statusOptions && canEdit ? (
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <button
                    type="button"
                    className="flex h-8 w-full max-w-sm items-center justify-between gap-2 rounded-md border border-input bg-background px-2.5 text-body transition-colors hover:bg-accent/50"
                  />
                }
              >
                {statusField}
                <ChevronDown aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="start" className="max-h-80 w-(--anchor-width) overflow-y-auto">
                {statusOptions.length === 0 ? (
                  <p className="px-2 py-1.5 text-caption text-muted-foreground">{t(($) => $.workflows.editor.no_other_status)}</p>
                ) : (
                  statusOptions.map((option) => (
                    <DropdownMenuItem key={option} onClick={() => onReplaceStatus(option)}>
                      {icon(option)}
                      <span className="truncate">{labelOf(option)}</span>
                    </DropdownMenuItem>
                  ))
                )}
              </DropdownMenuContent>
            </DropdownMenu>
          ) : (
            <div className="flex h-8 w-full max-w-sm items-center rounded-md border border-input bg-muted/40 px-2.5 text-body">
              {statusField}
            </div>
          )}
        </DetailField>
        <DetailField label={t(($) => $.workflows.editor.on_enter)}>
          {off ? (
            <p className="text-caption leading-5 text-muted-foreground">{t(($) => $.workflows.editor.step_off)}</p>
          ) : !key || statusAcceptsHandoff(key) ? (
            // A new status is a middle column, so it always takes a handoff;
            // the save creates the status and keeps the handoff on it.
            <HandoffFields
              step={{ status_key: columnKey(column), ...(handoff ?? NO_HANDOFF) }}
              workflow={flow}
              canEdit={canEdit}
              onPatch={onPatch}
              onPreview={onPreview}
              defaultNextLabel={defaultNextLabel}
            />
          ) : (
            <p className="text-caption leading-5 text-muted-foreground">{t(($) => $.workflows.editor.no_handoff_step)}</p>
          )}
        </DetailField>
      </div>
    </section>
  );
}

function DetailField({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div className="space-y-2">
      <p className="text-caption font-medium">{label}</p>
      {children}
      {hint && <p className="text-caption leading-5 text-muted-foreground">{hint}</p>}
    </div>
  );
}
