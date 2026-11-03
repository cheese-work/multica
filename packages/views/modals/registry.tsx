"use client";

import { lazy, Suspense, type ReactNode } from "react";
import { useModalStore } from "@multica/core/modals";
import { IssueLimitUpgradeDialog } from "./issue-limit-upgrade-dialog";

// Modals are opened on demand; keeping them (and the editor they bring) out of
// the initial dashboard bundle keeps first paint on the list pages light.
const CreateIssueDialog = lazy(() =>
  import("./create-issue-dialog").then((m) => ({ default: m.CreateIssueDialog })),
);
const CreateProjectModal = lazy(() =>
  import("./create-project").then((m) => ({ default: m.CreateProjectModal })),
);
const CreateSquadModal = lazy(() =>
  import("./create-squad").then((m) => ({ default: m.CreateSquadModal })),
);
const FeedbackModal = lazy(() =>
  import("./feedback").then((m) => ({ default: m.FeedbackModal })),
);
const SetParentIssueModal = lazy(() =>
  import("./set-parent-issue").then((m) => ({ default: m.SetParentIssueModal })),
);
const MarkDuplicateIssueModal = lazy(() =>
  import("./mark-duplicate-issue").then((m) => ({ default: m.MarkDuplicateIssueModal })),
);
const AddChildIssueModal = lazy(() =>
  import("./add-child-issue").then((m) => ({ default: m.AddChildIssueModal })),
);
const RenameIssueModal = lazy(() =>
  import("./rename-issue").then((m) => ({ default: m.RenameIssueModal })),
);
const DeleteIssueConfirmModal = lazy(() =>
  import("./delete-issue-confirm").then((m) => ({ default: m.DeleteIssueConfirmModal })),
);
const RunConfirmModal = lazy(() =>
  import("./run-confirm").then((m) => ({ default: m.RunConfirmModal })),
);
const WorkflowHandoffModal = lazy(() =>
  import("./workflow-handoff").then((m) => ({ default: m.WorkflowHandoffModal })),
);

export function ModalRegistry() {
  const modal = useModalStore((s) => s.modal);
  const data = useModalStore((s) => s.data);
  const close = useModalStore((s) => s.close);

  let activeModal: ReactNode = null;
  switch (modal) {
    // Both modal types open the same shell so the in-modal mode switch is
    // instant — only the inner panel swaps, the Dialog Root stays mounted.
    case "create-issue":
      activeModal = (
        <CreateIssueDialog
          onClose={close}
          initialMode="manual"
          data={data}
        />
      );
      break;
    case "quick-create-issue":
      activeModal = (
        <CreateIssueDialog
          onClose={close}
          initialMode="agent"
          data={data}
        />
      );
      break;
    case "create-project":
      activeModal = <CreateProjectModal onClose={close} />;
      break;
    case "create-squad":
      activeModal = <CreateSquadModal onClose={close} />;
      break;
    case "feedback":
      activeModal = <FeedbackModal onClose={close} data={data} />;
      break;
    case "issue-set-parent":
      activeModal = <SetParentIssueModal onClose={close} data={data} />;
      break;
    case "issue-mark-duplicate":
      activeModal = <MarkDuplicateIssueModal onClose={close} data={data} />;
      break;
    case "issue-add-child":
      activeModal = <AddChildIssueModal onClose={close} data={data} />;
      break;
    case "issue-rename":
      activeModal = <RenameIssueModal onClose={close} data={data} />;
      break;
    case "issue-delete-confirm":
      activeModal = <DeleteIssueConfirmModal onClose={close} data={data} />;
      break;
    case "issue-run-confirm":
      activeModal = <RunConfirmModal onClose={close} data={data} />;
      break;
    case "issue-workflow-handoff":
      activeModal = <WorkflowHandoffModal onClose={close} data={data} />;
      break;
  }

  return (
    <>
      <Suspense fallback={null}>{activeModal}</Suspense>
      <IssueLimitUpgradeDialog />
    </>
  );
}
