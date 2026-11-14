"use client";

import { useEffect, useRef, useState, type CSSProperties, type RefObject } from "react";
import { useWorkspacePaths } from "@multica/core/paths";
import type { Issue } from "@multica/core/types";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@multica/ui/components/ui/dialog";
import { useNavigation } from "../../../navigation";
import { useT } from "../../../i18n";
import { requestHierarchyFocus } from "./hierarchy-minimap";
import { ModeSwitch, useIssueMode, type IssueMode } from "./issue-modes";

const COLLAPSE_MS = 180;

/** Scale and origin that map `from` onto `to` along one axis: the origin is the one fixed point of the scaling. */
const squeeze = (fromStart: number, fromSize: number, toStart: number, toSize: number) => {
  const scale = toSize / fromSize;
  return { scale, origin: scale === 1 ? fromSize / 2 : (toStart - scale * fromStart) / (1 - scale) - fromStart };
};

export function HierarchyDialog({ issue, headingRef, modes, onClose }: { issue: Issue; headingRef: RefObject<HTMLHeadingElement | null>; modes: readonly IssueMode[]; onClose: () => void }) {
  const { t } = useT("issues");
  const paths = useWorkspacePaths();
  const navigation = useNavigation();
  const viewportRef = useRef<HTMLDivElement>(null);
  const { mode, setMode } = useIssueMode(modes);

  const popupRef = useRef<HTMLDivElement>(null);
  const [collapse, setCollapse] = useState<CSSProperties | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const pending = useRef<() => void>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  // D4: the dialog shrinks into the sidebar minimap, then navigates; the target's minimap takes focus.
  const open = (id: string) => {
    if (collapse) return;
    requestHierarchyFocus();
    const go = () => { onClose(); navigation.push(paths.issueDetail(id)); };
    const popup = popupRef.current?.getBoundingClientRect();
    const target = headingRef.current?.closest("section")?.getBoundingClientRect();
    if (!popup || !target || window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) return go();
    const x = squeeze(popup.left, popup.width, target.left, target.width);
    const y = squeeze(popup.top, popup.height, target.top, target.height);
    setCollapse({
      scale: `${x.scale} ${y.scale}`,
      transformOrigin: `${x.origin}px ${y.origin}px`,
      opacity: 0,
      transition: `scale ${COLLAPSE_MS}ms ease-in, opacity ${COLLAPSE_MS}ms ease-in`,
    });
    // transitionend keeps the shrink complete on a slow frame; the timer covers a transition that never fires.
    pending.current = go;
    timer.current = setTimeout(finish, COLLAPSE_MS * 2);
  };
  useEffect(() => {
    const popup = popupRef.current;
    if (!collapse || !popup) return;
    const onEnd = (e: TransitionEvent) => { if (e.target === popup && e.propertyName === "opacity") finish(); };
    popup.addEventListener("transitionend", onEnd);
    return () => popup.removeEventListener("transitionend", onEnd);
  });
  const finish = () => {
    clearTimeout(timer.current);
    const go = pending.current;
    pending.current = undefined;
    go?.();
  };

  return (
    <Dialog open onOpenChange={(next) => { if (!next) { clearTimeout(timer.current); pending.current = undefined; onClose(); } }}>
      <DialogContent ref={popupRef} overlayClassName="supports-backdrop-filter:backdrop-blur-none" style={collapse ?? undefined} data-testid="hierarchy-dialog" initialFocus={viewportRef} finalFocus={headingRef} className="flex h-[min(48rem,90dvh)] flex-col gap-3 sm:max-w-6xl">
        <DialogHeader className="flex-row flex-wrap items-center gap-x-3 gap-y-2 pr-8">
          <DialogTitle>{t(($) => $.hierarchy.title)} <span aria-hidden="true" className="ml-2 font-mono text-caption text-muted-foreground">{issue.identifier}</span></DialogTitle>
          <DialogDescription className="sr-only">{t(($) => $.hierarchy.dialog_description)}</DialogDescription>
          <ModeSwitch modes={modes} activeId={mode.id} onChange={setMode} />
        </DialogHeader>
        <mode.Dialog issue={issue} viewportRef={viewportRef} onOpen={open} />
      </DialogContent>
    </Dialog>
  );
}

