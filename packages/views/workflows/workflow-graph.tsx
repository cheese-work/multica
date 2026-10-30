"use client";

import { useCallback, useMemo, useState } from "react";
import {
  Background,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  type Dimensions,
  type Edge,
  type Node,
  type NodeChange,
  type NodeProps,
  type XYPosition,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import {
  addStepEdge,
  workflowEdges,
} from "@multica/core/issue-workflows";
import type {
  IssueWorkflow,
  IssueWorkflowStep,
  Project,
} from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { ActorAvatar } from "../common/actor-avatar";
import { StatusIcon } from "../issues/components/status-icon";
import { useStatusLabel } from "../issues/utils/status-label";
import { useT } from "../i18n";
import { stepHandlerActor, useStepHandlerLabel } from "./step-handler";

const NODE_WIDTH = 180;
const COLUMN_GAP = 90;
const ROW_GAP = 110;

type StepNodeData = {
  step: IssueWorkflowStep;
  label: string;
  handler: string | null;
  current: boolean;
  editable: boolean;
  project: Pick<Project, "lead_type" | "lead_id"> | null;
};

function StepNode({ data }: NodeProps<Node<StepNodeData>>) {
  const catalog = useIssueStatuses(useWorkspaceId());
  const { step, label, handler, current, editable, project } = data;
  const actor = stepHandlerActor(step, project);
  return (
    <div
      aria-current={current ? "step" : undefined}
      style={{ width: NODE_WIDTH }}
      className={cn(
        "rounded-lg border bg-card px-3 py-2 text-card-foreground",
        current
          ? "border-primary ring-2 ring-primary/30"
          : "border-surface-border",
      )}
    >
      <Handle type="target" position={Position.Left} isConnectable={editable} />
      <div className="flex items-center gap-2">
        <StatusIcon
          status={step.status_key}
          category={catalog.categoryOf(step.status_key)}
          color={catalog.colorOf(step.status_key)}
          icon={catalog.iconOf(step.status_key)}
          className="h-3.5 w-3.5 shrink-0"
        />
        <span
          className={cn(
            "min-w-0 flex-1 truncate text-body",
            current && "font-medium",
          )}
        >
          {label}
        </span>
      </div>
      {handler && (
        <div className="mt-1 flex min-w-0 items-center gap-1 text-caption text-muted-foreground">
          {actor && (
            <ActorAvatar
              actorType={actor.type}
              actorId={actor.id}
              size="xs"
              profileLink={false}
            />
          )}
          <span className="truncate">{handler}</span>
        </div>
      )}
      <Handle type="source" position={Position.Right} isConnectable={editable} />
    </div>
  );
}

const nodeTypes = { step: StepNode };

/**
 * A flowchart of a workflow (CHE-2150): a node per step, an arrow per exit
 * labelled with its guard. Steps run left to right in column order, staggered
 * so an arrow that skips a step clears it. The exits also render as a list,
 * which is what assistive tech and tests read.
 *
 * Read-only unless `onChange` is given. Then nodes can be dragged (the layout
 * is not saved) and a drag from one node's handle to another adds an exit;
 * everything else is edited in the exits form.
 */
export function WorkflowGraph({
  workflow,
  currentStatus,
  project,
  className,
  onChange,
  onSelectStep,
}: {
  workflow: Pick<IssueWorkflow, "steps">;
  /** The status the issue sits on, or the step being edited; highlighted. */
  currentStatus?: string;
  project: Pick<Project, "lead_type" | "lead_id"> | null;
  className?: string;
  /** Makes the graph editable; receives the steps with the new exit. */
  onChange?: (steps: IssueWorkflowStep[]) => void;
  onSelectStep?: (statusKey: string) => void;
}) {
  const { t } = useT("issues");
  const labelOf = useStatusLabel(useWorkspaceId());
  const handlerLabel = useStepHandlerLabel(project);
  const edges = useMemo(() => workflowEdges(workflow), [workflow]);

  const editable = !!onChange;
  const [moved, setMoved] = useState<Record<string, XYPosition>>({});
  // React Flow keeps a controlled node hidden, and draws no edge to it, until
  // the measured size comes back through `onNodesChange`.
  const [measured, setMeasured] = useState<Record<string, Dimensions>>({});
  const onNodesChange = useCallback((changes: NodeChange[]) => {
    setMoved((m) => {
      const next = { ...m };
      for (const c of changes) {
        if (c.type === "position" && c.position) next[c.id] = c.position;
      }
      return next;
    });
    setMeasured((m) => {
      const next = { ...m };
      for (const c of changes) {
        if (c.type === "dimensions" && c.dimensions) next[c.id] = c.dimensions;
      }
      return next;
    });
  }, []);

  const nodes = useMemo<Node<StepNodeData>[]>(
    () =>
      workflow.steps.map((step, i) => ({
        id: step.status_key,
        type: "step",
        position: moved[step.status_key] ?? {
          x: i * (NODE_WIDTH + COLUMN_GAP),
          y: (i % 2) * ROW_GAP,
        },
        data: {
          step,
          label: labelOf(step.status_key),
          handler: handlerLabel(step),
          current: step.status_key === currentStatus,
          editable,
          project,
        },
        measured: measured[step.status_key],
        draggable: editable,
      })),
    [workflow.steps, labelOf, handlerLabel, currentStatus, project, editable, moved, measured],
  );

  const flowEdges = useMemo<Edge[]>(
    () =>
      edges.map((e) => ({
        id: e.id,
        source: e.from,
        target: e.to,
        label: e.timeout ? `${e.guard} · ${e.timeout}` : e.guard,
        type: "default",
        markerEnd: { type: MarkerType.ArrowClosed },
        labelStyle: { fill: "var(--color-foreground)", fontSize: 11 },
        labelBgStyle: { fill: "var(--color-card)" },
      })),
    [edges],
  );

  return (
    <div className={cn("space-y-2", className)}>
      <div
        role="img"
        aria-label={t(($) => $.workflows.flow.graph_label)}
        data-testid="workflow-graph"
        className="h-72 rounded-lg border border-surface-border"
      >
        <ReactFlow
          nodes={nodes}
          edges={flowEdges}
          nodeTypes={nodeTypes}
          fitView
          nodesDraggable={editable}
          nodesConnectable={editable}
          elementsSelectable={editable}
          onNodesChange={onNodesChange}
          onNodeClick={(_, node) => onSelectStep?.(node.id)}
          onEdgeClick={(_, edge) => onSelectStep?.(edge.source)}
          onConnect={({ source, target }) => {
            if (onChange && source && target && source !== target) {
              onChange(addStepEdge(workflow.steps, source, target));
            }
          }}
          proOptions={{ hideAttribution: false }}
        >
          <Background />
        </ReactFlow>
      </div>
      <ol
        aria-label={t(($) => $.workflows.flow.exits_label)}
        className="sr-only"
      >
        {edges.map((e) => (
          <li key={e.id}>
            {t(($) => $.workflows.flow.exit_item, {
              from: labelOf(e.from),
              to: labelOf(e.to),
              guard: e.timeout ? `${e.guard} (${e.timeout})` : e.guard,
            })}
          </li>
        ))}
      </ol>
    </div>
  );
}
