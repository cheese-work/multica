"use client";

import { useMemo } from "react";
import {
  Background,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  type Edge,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { workflowEdges } from "@multica/core/issue-workflows";
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
  project: Pick<Project, "lead_type" | "lead_id"> | null;
};

function StepNode({ data }: NodeProps<Node<StepNodeData>>) {
  const catalog = useIssueStatuses(useWorkspaceId());
  const { step, label, handler, current, project } = data;
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
      <Handle type="target" position={Position.Left} isConnectable={false} />
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
      <Handle type="source" position={Position.Right} isConnectable={false} />
    </div>
  );
}

const nodeTypes = { step: StepNode };

/**
 * A read-only flowchart of a workflow (CHE-2150): a node per step, an arrow
 * per exit labelled with its guard. Steps run left to right in column order,
 * staggered so an arrow that skips a step clears it. The exits also render as
 * a list, which is what assistive tech and tests read.
 */
export function WorkflowGraph({
  workflow,
  currentStatus,
  project,
  className,
}: {
  workflow: Pick<IssueWorkflow, "steps">;
  /** The status the issue sits on; highlighted. */
  currentStatus?: string;
  project: Pick<Project, "lead_type" | "lead_id"> | null;
  className?: string;
}) {
  const { t } = useT("issues");
  const labelOf = useStatusLabel(useWorkspaceId());
  const handlerLabel = useStepHandlerLabel(project);
  const edges = useMemo(() => workflowEdges(workflow), [workflow]);

  const nodes = useMemo<Node<StepNodeData>[]>(
    () =>
      workflow.steps.map((step, i) => ({
        id: step.status_key,
        type: "step",
        position: { x: i * (NODE_WIDTH + COLUMN_GAP), y: (i % 2) * ROW_GAP },
        data: {
          step,
          label: labelOf(step.status_key),
          handler: handlerLabel(step),
          current: step.status_key === currentStatus,
          project,
        },
        draggable: false,
      })),
    [workflow.steps, labelOf, handlerLabel, currentStatus, project],
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
          nodesDraggable={false}
          nodesConnectable={false}
          elementsSelectable={false}
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
