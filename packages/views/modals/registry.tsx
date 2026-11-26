"use client";

<<<<<<< HEAD
import { lazy, Suspense, type ReactNode } from "react";
||||||| 2fcb424b72
import type { ReactNode } from "react";
=======
import { useState, type ReactNode } from "react";
>>>>>>> 35b54a3742bb
import { useModalStore } from "@multica/core/modals";
<<<<<<< HEAD
import { IssueLimitUpgradeDialog } from "./issue-limit-upgrade-dialog";
||||||| 2fcb424b72
import { CreateIssueDialog } from "./create-issue-dialog";
import { CreateProjectModal } from "./create-project";
import { CreateSquadModal } from "./create-squad";
import { FeedbackModal } from "./feedback";
import { SetParentIssueModal } from "./set-parent-issue";
import { MarkDuplicateIssueModal } from "./mark-duplicate-issue";
import { AddChildIssueModal } from "./add-child-issue";
import { AddProjectIssueModal } from "./add-project-issue";
import { RenameIssueModal } from "./rename-issue";
import { RenameProjectModal } from "./rename-project";
import { DeleteIssueConfirmModal } from "./delete-issue-confirm";
import { RunConfirmModal } from "./run-confirm";
import { WorkflowHandoffModal } from "./workflow-handoff";
import { IssueLimitUpgradeDialog } from "./issue-limit-upgrade-dialog";
=======
import { CreateIssueDialog } from "./create-issue-dialog";
import { CreateProjectModal } from "./create-project";
import { CreateSquadModal } from "./create-squad";
import { FeedbackModal } from "./feedback";
import { SetParentIssueModal } from "./set-parent-issue";
import { MarkDuplicateIssueModal } from "./mark-duplicate-issue";
import { AddChildIssueModal } from "./add-child-issue";
import { AddBlockerIssueModal } from "./add-blocker-issue";
import { AddProjectIssueModal } from "./add-project-issue";
import { RenameIssueModal } from "./rename-issue";
import { RenameProjectModal } from "./rename-project";
import { getProjectFoldersExtension } from "../extensions";
import { DeleteIssueConfirmModal } from "./delete-issue-confirm";
import { RunConfirmModal } from "./run-confirm";
import { WorkflowHandoffModal } from "./workflow-handoff";
import { LimitRecoveryDialog } from "./limit-recovery-dialog";

type ActiveModalName = NonNullable<ReturnType<typeof useModalStore.getState>["modal"]>;

type RetainedModal = {
  modal: ActiveModalName;
  data: Record<string, unknown> | null;
  sessionKey: number;
  closing: boolean;
};
>>>>>>> 35b54a3742bb

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
const AddProjectIssueModal = lazy(() =>
  import("./add-project-issue").then((m) => ({ default: m.AddProjectIssueModal })),
);
const RenameIssueModal = lazy(() =>
  import("./rename-issue").then((m) => ({ default: m.RenameIssueModal })),
);
const RenameProjectModal = lazy(() =>
  import("./rename-project").then((m) => ({ default: m.RenameProjectModal })),
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
  const [retained, setRetained] = useState<RetainedModal | null>(null);

  if (modal !== null) {
    if (
      retained === null ||
      retained.closing ||
      retained.modal !== modal ||
      retained.data !== data
    ) {
      setRetained({
        modal,
        data,
        sessionKey: (retained?.sessionKey ?? 0) + 1,
        closing: false,
      });
    }
  } else if (retained !== null && !retained.closing) {
    setRetained({ ...retained, closing: true });
  }

  const snapshot: RetainedModal | null =
    modal !== null
      ? retained !== null &&
        !retained.closing &&
        retained.modal === modal &&
        retained.data === data
        ? retained
        : {
            modal,
            data,
            sessionKey: (retained?.sessionKey ?? 0) + 1,
            closing: false,
          }
      : retained;

  const isOpen = modal !== null;

  let activeModal: ReactNode = null;
  if (snapshot !== null) {
    const { modal: currentModal, data: currentData, sessionKey } = snapshot;
    const handleOpenChangeComplete = (nextOpen: boolean) => {
      if (!nextOpen) {
        setRetained((prev) =>
          prev !== null && prev.sessionKey === sessionKey && prev.closing
            ? null
            : prev,
        );
      }
    };

    switch (currentModal) {
      // Both modal types open the same shell so the in-modal mode switch is
      // instant — only the inner panel swaps, the Dialog Root stays mounted.
      case "create-issue":
        activeModal = (
          <CreateIssueDialog
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            initialMode="manual"
            data={currentData}
          />
        );
        break;
      case "quick-create-issue":
        activeModal = (
          <CreateIssueDialog
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            initialMode="agent"
            data={currentData}
          />
        );
        break;
      case "create-project":
        activeModal = (
          <CreateProjectModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
      case "create-squad":
        activeModal = (
          <CreateSquadModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
          />
        );
        break;
      case "feedback":
        activeModal = (
          <FeedbackModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
      case "issue-set-parent":
        activeModal = (
          <SetParentIssueModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
      case "issue-mark-duplicate":
        activeModal = (
          <MarkDuplicateIssueModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
      case "issue-add-child":
        activeModal = (
          <AddChildIssueModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
      case "issue-add-blocker":
      case "issue-add-blocking":
        activeModal = (
          <AddBlockerIssueModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
            direction={currentModal === "issue-add-blocking" ? "blocking" : "blocked_by"}
          />
        );
        break;
      case "project-add-issue":
        activeModal = (
          <AddProjectIssueModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
      case "project-rename":
        activeModal = (
          <RenameProjectModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
      case "issue-rename":
        activeModal = (
          <RenameIssueModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
      case "issue-delete-confirm":
        activeModal = (
          <DeleteIssueConfirmModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
      case "issue-run-confirm":
        activeModal = (
          <RunConfirmModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
      case "issue-workflow-handoff":
        activeModal = (
          <WorkflowHandoffModal
            key={sessionKey}
            open={isOpen}
            onClose={close}
            onOpenChangeComplete={handleOpenChangeComplete}
            data={currentData}
          />
        );
        break;
    }
  }

  const FoldersHost = getProjectFoldersExtension()?.Host;

  return (
    <>
<<<<<<< HEAD
      <Suspense fallback={null}>{activeModal}</Suspense>
      <IssueLimitUpgradeDialog />
||||||| 2fcb424b72
      {activeModal}
      <IssueLimitUpgradeDialog />
=======
      {activeModal}
      <LimitRecoveryDialog />
      {FoldersHost && <FoldersHost />}
>>>>>>> 35b54a3742bb
    </>
  );
}
