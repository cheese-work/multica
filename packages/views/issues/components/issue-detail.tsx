"use client";

import { RunBudgetMeter } from "./run-budget-meter";
import { subIssueCreateDefaults } from "@multica/core/issues";
import {
  issueBehavesAs,
  issueBehavesAsAny,
  issueStatusCategory,
} from "@multica/core/issues";
import { IssueFlowCard, useIssueFlowVisible } from "../../workflows/issue-flow-card";
import { IssueAppRequests } from "../../common/app-requests";
import { useStatusLabel } from "../utils/status-label";
import { formatActivity, latestSquadHandoffRound, squadHandoffNote, type ActivityT } from "./activity-text";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { useState, useEffect, useLayoutEffect, useCallback, useMemo, useRef, Fragment, type ComponentProps, type ReactNode } from "react";
import { AnimatePresence } from "motion/react";
import { UI_MOTION_DURATION } from "@multica/ui/lib/motion";
import { Virtuoso, type ListRange, type VirtuosoHandle } from "react-virtuoso";
import { RichContentScrollRootProvider } from "../../rich-content/scroll-root";
import { NextSelfMention, useMentionsViewer } from "../../rich-content/next-self-mention";
import { MediaSourceProvider } from "../../media/media-context";
import { useDefaultLayout, usePanelRef } from "react-resizable-panels";
import type { ReplyAnnotation } from "@multica/core/drafts/reply-annotation";
import { usePersistedDraft } from "@multica/core/drafts";
import { AppLink, useBackOrReplace } from "../../navigation";
import { IssueRelationsSection } from "./issue-blockers";
import { IssueDuplicateBanner, isDuplicateIssue } from "./issue-duplicates";
import {
  AlertTriangle,
  Calendar,
  CalendarClock,
  Unlink,
  FolderKanban,
  CalendarDays,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  FileQuestion,
  Loader2,
  Lock,
  Milestone,
  MoreHorizontal,
  PanelRight,
  Plus,
  SlidersHorizontal,
  Tag,
  Users,
  ArrowUpRight,
} from "@multica/ui/icons";
import { BreadcrumbHeader, type BreadcrumbSegment } from "../../layout/breadcrumb-header";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { Button } from "@multica/ui/components/ui/button";
import { IconButton } from "@multica/ui/components/common/icon-button";
import { ResizablePanelGroup, ResizablePanel, ResizableHandle } from "@multica/ui/components/ui/resizable";
import { Sheet, SheetContent } from "@multica/ui/components/ui/sheet";
import { useIsMobile } from "@multica/ui/hooks/use-mobile";
import { StorageLimitNotice } from "../../billing/storage-limit-notice";
import { useDocCollab, type DocCollaborator } from "../../docs/components/use-doc-collab";
import { ContentEditor, ReadonlyContent, type ContentEditorRef, TitleEditor, type TitleEditorRef, useFileDropZone, FileDropOverlay, useLazyEditor, useEditorUpload, useUploadGate, PreviewSequenceProvider, collectPreviewSequence, ImageThumbnailsProvider } from "../../editor";
import {
  WAKEUP_ACTIVITY_ACTIONS,
  WakeupActivityIcon,
  WakeupActivityLinks,
  formatWakeupActivity,
  wakeupActivityChip,
} from "./wakeup-activity";
import { useWakeupText } from "./wakeup-presentation";
import { NEVER_COALESCE_ACTIONS, NO_TIME_LIMIT_ACTIONS } from "./timeline-coalesce";
import type { ImageSequenceBlock } from "@multica/core/attachments/image-sequence";
import { DescriptionDisclosure } from "./description-disclosure";
import { StatusSummary, statusSummaryOf } from "./status-summary";
import { IssueDecisionBanner } from "./issue-decision-banner";
import { FileUploadButton } from "@multica/ui/components/common/file-upload-button";
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@multica/ui/components/ui/tooltip";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
} from "@multica/ui/components/ui/dropdown-menu";
import { Popover, PopoverTrigger, PopoverContent } from "@multica/ui/components/ui/popover";
import { KeyUpdatesSubscriptionBadge } from "./key-updates-subscription-badge";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { Command, CommandInput, CommandList, CommandEmpty, CommandGroup, CommandItem } from "@multica/ui/components/ui/command";
import { AvatarGroup, AvatarGroupCount } from "@multica/ui/components/ui/avatar";
import { ActorAvatar } from "../../common/actor-avatar";
import { ActionShortcutKeycaps } from "../../common/shortcut-keycaps";
import { COMPOSER_DOCK_GAP, ComposerDock, JumpToLatestButton } from "./composer-dock";
import { InboxIssueActions, IssueBarActions } from "./inbox-issue-actions";
import { useInboxSelectionStore, type InboxTransitionDirection } from "@multica/core/inbox/selection-store";
import { useReducedMotion } from "@multica/core/appearance";
import { PropRow } from "../../common/prop-row";
import { useIsGuest } from "@multica/core/permissions";
import { TimelineHandoff } from "../../workflows/timeline-handoff";
import { PropertyIcon } from "../../common/property-icon";
import type { Attachment, InboxItem, Issue, IssueProperty, IssueStatus, IssueStatusCategory, IssuePriority, IssueSubscriber, TimelineEntry, UpdateIssueRequest } from "@multica/core/types";
import { contentReferencesAttachment } from "@multica/core/types";
import { commentLandingTarget, isDeletedComment } from "@multica/core/issues/comment-deletion";
import { formatDateOnly, isPastDateOnly } from "@multica/core/issues/date";
import { useUpdateIssue } from "@multica/core/issues/mutations";
import { toast } from "sonner";
import { errorCode } from "@multica/core/api";
import { StatusIcon } from "./status-icon";
import { PriorityIcon } from "./priority-icon";
import { StatusPicker } from "./pickers/status-picker";
import { PriorityPicker } from "./pickers/priority-picker";
import { StagePicker, maxSiblingStage } from "./pickers/stage-picker";
import { StartDatePicker } from "./pickers/start-date-picker";
import { DueDatePicker } from "./pickers/due-date-picker";
import { AssigneePicker } from "./pickers/assignee-picker";
import { LabelPicker } from "./pickers/label-picker";
import { CustomPropertyValueEditor, CustomPropertyValueDisplay } from "./pickers/custom-property-picker";
import { Switch } from "@multica/ui/components/ui/switch";
import { IssueActionsDropdown, useIssuePageMenu, useIssueActions, useArchivedInboxItemId, IssueActionsContextMenu, IssueContextMenuProvider } from "../actions";
import { LabelChip } from "../../labels/label-chip";
import { IssueAgentActivityIndicator } from "./issue-agent-activity-indicator";
import { SubIssuesAgentWorkingChip } from "./sub-issues-agent-working-chip";
import { IssueProjectPicker } from "../../projects/components/issue-project-picker";
import { IssuePropertyPills } from "./issue-property-pills";
import { LocalDirectoryHint } from "../../projects/components/local-directory-hint";
import { revealElement, useNewRunIds } from "./use-run-comment-motion";
import { AgentRunComment, CommentCard } from "./comment-card";
import { AttachmentList } from "./attachment-list";
import { chainedIssueAttachments } from "@multica/core/attachments/revision-diff";
import { projectCommentThread, type CommentThreadRow } from "./comment-thread-rows";
import { EMPTY_COMMENT_RUNS, buildCommentRunView, isActiveCommentRun, orderTimelineWithRuns, type CommentRun } from "./comment-runs";
import { issueTasksOptions } from "@multica/core/issues/queries";
import { SourceContextBadge } from "./source-context-viewer";
import { RevisionConflictCompare } from "./revision-conflict-compare";
import { CommentInput } from "./comment-input";
import { useCommentAnnotations } from "./use-comment-annotations";
import { CurrentIssueRenderContextProvider } from "../current-issue-render-context";
import { ResolvedThreadBar } from "./resolved-thread-bar";
import { SupersessionRevisionsProvider } from "./supersession-links";
import {
  ThreadMinimap,
  createRowRangeSource,
  type MountedRows,
  type ThreadMinimapThread,
} from "./thread-minimap";
import { collectThreadParticipants, collectThreadReplies, deriveThreadResolution } from "./thread-utils";
import { IssueAgentHeaderChip } from "./issue-agent-header-chip";
import { IssuePresenceHeaderChip } from "./issue-presence-header-chip";
import { IssueWakeupHeaderChip } from "./issue-wakeup-header-chip";
import { useWakeupsFocus } from "./use-wakeups-focus";
import { ExecutionLogSection } from "./execution-log-section";
import { StatusHistoryButton } from "./status-history-button";
import { WakeupsSection } from "./wakeups-section";
import { QuickActionsSection } from "./quick-actions-section";
import { PluginPanelSection } from "../../plugins";
import { PullRequestsSection } from "./pull-requests-section";
import { IssueEnvironmentsSection } from "./issue-environments-section";
import { useGitHubSettings } from "@multica/core/github";
import { useFeatureEnabled } from "@multica/core/config";
import { PLUGINS_V1_FLAG } from "@multica/core/feature-flags";
import { DeliverablesSection } from "./deliverables/deliverables-section";
import { IssueAppsSection } from "./issue-apps-section";
import { DeliverablesOverview } from "./deliverables/deliverables-overview";
import {
  DESCRIPTION_BLOCK_ID,
  useDeliverableDetails,
  type DeliverableOrigin,
} from "./deliverables/deliverable-details";
import { collectDeliverableFiles, deliverableInventoryFromAttachments, mergeDeliverableSources } from "@multica/core/attachments/deliverables";
import { AttachmentVersionsProvider } from "./deliverables/attachment-versions";
import {
  OpenDeliverableOnLoad,
  useCopyDeliverableLink,
  useDeliverableLinkSession,
  useFreshTimelineRead,
} from "./deliverables/deliverable-link";
import { useQuery } from "@tanstack/react-query";
import { useIssueReadState } from "@multica/core/issues/read-state";
import { useIssueRouteRef } from "@multica/core/issues/canonical-id";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspacePaths, issueRouteRef } from "@multica/core/paths";
import { useActorName } from "@multica/core/workspace/hooks";
import { useWorkspaceId } from "@multica/core/hooks";
import { IssueHierarchy } from "./hierarchy/hierarchy-minimap";
import { useRecentContextStore } from "@multica/core/chat";
import { useRegisterCommandBarContext } from "../../search/register-context";
import { useModalStore } from "@multica/core/modals";
import { issueListOptions, issueDetailOptions, issueDetailViewOptions, childIssuesOptions, childIssueProgressOptions, issueAttachmentsOptions } from "@multica/core/issues/queries";
import { projectDetailOptions } from "@multica/core/projects/queries";
import { ProjectIcon } from "../../projects/components/project-icon";
import { issueLabelsOptions } from "@multica/core/labels";
import { propertyListOptions } from "@multica/core/properties";
import { memberListOptions, agentListOptions } from "@multica/core/workspace/queries";
import {
  selectExpandedResolved,
  selectExpandedThreads,
  selectDescriptionExpanded,
  selectJustFoldedRoots,
  useCommentDraftStore,
  useIssueDisclosureStore,
  useCommentCollapseStore,
  useCommentComposerStore,
  useRecentIssuesStore,
  useResolvedExpandStore,
  useSubIssuesCollapseStore,
  useSubIssueDisplayStore,
  useIssueTitleDraftStore,
  EMPTY_TEXT_EDIT,
  SUB_ISSUE_ROW_PROPERTY_KEYS,
  type SubIssueRowProperties,
  type SubIssueRowPropertyKey,
  ISSUE_SIDEBAR_SECTIONS,
  type IssueSidebarSection,
} from "@multica/core/issues/stores";
import { BatchActionToolbar } from "./batch-action-toolbar";
import {
  IssueSurfaceSelectionProvider,
  useCreateIssueSurfaceSelection,
  useIssueSurfaceSelection,
} from "../surface/selection-context";
import { useIssueTimeline } from "../hooks/use-issue-timeline";
import { useIssueReactions } from "../hooks/use-issue-reactions";
import { useIssueSubscribers } from "../hooks/use-issue-subscribers";
import { useMarkIssueReadWhileViewing } from "../hooks/use-mark-issue-read-while-viewing";
import { useMarkIssueInboxReadWhileViewing } from "../hooks/use-mark-issue-inbox-read-while-viewing";
import { ReactionBar } from "@multica/ui/components/common/reaction-bar";
import { useLocale, useTimeAgo } from "../../i18n";
import {
  useRestoredScrollOffset,
  useRestoredScrollRef,
  useRestoredViewState,
  useViewStateWriter,
} from "../../platform";
import { cn } from "@multica/ui/lib/utils";
import { TruncatedTooltip } from "@multica/ui/components/common/truncated-tooltip";
import { CollectionPageState } from "../../layout/collection-page";
import { PAGE_GUTTER, usePageHeaderTitleBar } from "../../layout/page-header";
import { READING_COLUMN } from "../../layout/reading-column";
import { useShowInSidebar } from "../../layout/panels/show-in-sidebar";

import { ProgressRing } from "./progress-ring";
import { IssueSidebarAddRow, useIssueSidebarAddables, type IssueSidebarAddable } from "./issue-sidebar-add-row";
import { IssueSidebarFooter } from "./issue-sidebar-footer";
import { SystemWakeupChips } from "./system-wakeup-chip";
import { matchesPinyin } from "../../editor/extensions/pinyin-match";
import { MetadataSection } from "./metadata-section";
import { IssueSidebarCollapsible, IssueSidebarSectionHeader, IssueSidebarPropertyGrid } from "./issue-sidebar-section";
import { IssueSidebarSections, letSidebarSettingsDragKeysThrough } from "./issue-sidebar-settings";
import { useT } from "../../i18n";
import { useIssueDetailScrollRestore } from "../hooks/use-issue-detail-scroll-restore";
import { useJumpToLatest } from "../hooks/use-jump-to-latest";
import { useIssueDetailScrollAnchor } from "../hooks/use-issue-detail-scroll-anchor";
import { useNewCommentsWhileAway } from "../hooks/use-new-comments-while-away";
import { useInPageFind } from "../hooks/use-in-page-find";
import { useIssueDisclosureReveal } from "../hooks/use-issue-disclosure-reveal";
import { useStickyComposer } from "../hooks/use-sticky-composer";
import { FindBar } from "./find-bar";
import { issueHighlightMementoKey } from "./issue-highlight-key";
import {
  AnimatedRightSidebar,
  getAnimatedRightSidebarInitialOpen,
  rightSidebarPanelMotionProps,
  useAnimatedRightSidebarState,
  useRightSidebarShortcut,
} from "../../layout/animated-right-sidebar";

export { issueHighlightMementoKey };

// Shared by the subscribe button and the unsubscribe menu trigger so the two
// stay one control visually — they occupy the same slot and only differ in
// what a click does.
const SUBSCRIPTION_ACTION_CLASS =
  "text-label text-muted-foreground hover:text-foreground transition-colors disabled:pointer-events-none disabled:opacity-50";

function SubscriberAvatars({ subscribers }: { subscribers: IssueSubscriber[] }) {
  return (
    <AvatarGroup>
      {subscribers.slice(0, 4).map((sub) => (
        <ActorAvatar
          key={`${sub.user_type}-${sub.user_id}`}
          actorType={sub.user_type}
          actorId={sub.user_id}
          size="md"
          enableHoverCard
        />
      ))}
      {subscribers.length > 4 && (
        <AvatarGroupCount>+{subscribers.length - 4}</AvatarGroupCount>
      )}
    </AvatarGroup>
  );
}

function SubscriberPopoverContent({
  members,
  agents,
  subscribers,
  toggleSubscriber,
  togglesDisabled,
  t,
}: {
  members: { user_id: string; name: string }[];
  agents: { id: string; name: string; archived_at?: string | null }[];
  subscribers: { user_type: string; user_id: string }[];
  toggleSubscriber: (id: string, type: "member" | "agent", subscribed: boolean) => void;
  /**
   * Every checkbox here is drawn from `subscribers`, which defaults to an empty
   * list until the query resolves — so an unresolved query renders everyone as
   * unsubscribed. Acting on that is not a harmless no-op: an explicit subscribe
   * rewrites the target's reason to 'manual' and clears any opt-out scope
   * (server/pkg/db/queries/subscriber.sql), which would quietly discard a
   * delegated subscription or someone's deliberate opt-out. So these rows wait
   * for a real answer, not just for the in-flight mutation (MUL-5714).
   */
  togglesDisabled: boolean;
  t: ActivityT;
}) {
  const [search, setSearch] = useState("");
  const q = search.trim().toLowerCase();

  const uniqueMembers = members.filter((m, i, arr) => arr.findIndex((x) => x.user_id === m.user_id) === i);
  const activeAgents = agents.filter((a) => !a.archived_at);

  const filteredMembers = q
    ? uniqueMembers.filter((m) => m.name.toLowerCase().includes(q) || matchesPinyin(m.name, q))
    : uniqueMembers;
  const filteredAgents = q
    ? activeAgents.filter((a) => a.name.toLowerCase().includes(q) || matchesPinyin(a.name, q))
    : activeAgents;

  return (
    <PopoverContent align="end" className="w-64 p-0">
      <Command shouldFilter={false}>
        <CommandInput
          placeholder={t(($) => $.detail.change_subscribers_placeholder)}
          value={search}
          onValueChange={setSearch}
        />
        <CommandList className="max-h-64">
          {filteredMembers.length === 0 && filteredAgents.length === 0 && (
            <CommandEmpty>{t(($) => $.detail.no_subscribers_results)}</CommandEmpty>
          )}
          {filteredMembers.length > 0 && (
            <CommandGroup heading={t(($) => $.detail.members_group)}>
              {filteredMembers.map((m) => {
                const sub = subscribers.find((s) => s.user_type === "member" && s.user_id === m.user_id);
                const isSubbed = !!sub;
                return (
                  <CommandItem
                    key={`member-${m.user_id}`}
                    onSelect={() => toggleSubscriber(m.user_id, "member", isSubbed)}
                    disabled={togglesDisabled}
                    className="flex items-center gap-2.5"
                  >
                    <Checkbox checked={isSubbed} className="pointer-events-none" />
                    <ActorAvatar actorType="member" actorId={m.user_id} size="md" />
                    <span className="truncate flex-1">{m.name}</span>
                  </CommandItem>
                );
              })}
            </CommandGroup>
          )}
          {filteredAgents.length > 0 && (
            <CommandGroup heading={t(($) => $.detail.agents_group)}>
              {filteredAgents.map((a) => {
                const sub = subscribers.find((s) => s.user_type === "agent" && s.user_id === a.id);
                const isSubbed = !!sub;
                return (
                  <CommandItem
                    key={`agent-${a.id}`}
                    onSelect={() => toggleSubscriber(a.id, "agent", isSubbed)}
                    disabled={togglesDisabled}
                    className="flex items-center gap-2.5"
                  >
                    <Checkbox checked={isSubbed} className="pointer-events-none" />
                    <ActorAvatar actorType="agent" actorId={a.id} size="md" showStatusDot />
                    <span className="truncate flex-1">{a.name}</span>
                  </CommandItem>
                );
              })}
            </CommandGroup>
          )}
        </CommandList>
      </Command>
    </PopoverContent>
  );
}

function shortDate(date: string | null, locale: string): string {
  if (!date) return "—";
  return formatDateOnly(date, { month: "short", day: "numeric" }, locale);
}

/**
 * The issue a duplicate-mark activity (MUL-7349) names, when it can still be
 * opened. A mark removed because its original was deleted has nowhere to
 * link, so it renders as plain text.
 */
function duplicateActivityLink(entry: TimelineEntry): { id: string; identifier: string } | null {
  const details = (entry.details ?? {}) as Record<string, string>;
  switch (entry.action) {
    case "duplicate_marked":
    case "duplicate_unmarked":
      if (details.reason === "original_deleted") return null;
      return details.original_id && details.original_identifier
        ? { id: details.original_id, identifier: details.original_identifier }
        : null;
    case "duplicate_added":
    case "duplicate_removed":
      return details.duplicate_id && details.duplicate_identifier
        ? { id: details.duplicate_id, identifier: details.duplicate_identifier }
        : null;
    default:
      return null;
  }
}

/** Activity copy with the issue it names turned into a link. */
function ActivityText({ entry, text }: { entry: TimelineEntry; text: string }) {
  const paths = useWorkspacePaths();
  const link = duplicateActivityLink(entry);
  // The identifier in the activity is a snapshot; the cache knows the current one.
  const linkRef = useIssueRouteRef(useWorkspaceId(), link?.id ?? "");
  const at = link ? text.indexOf(link.identifier) : -1;
  if (!link || at < 0) return <>{text}</>;
  return (
    <>
      {text.slice(0, at)}
      <AppLink
        href={paths.issueDetail(linkRef)}
        newTabTitle={link.identifier}
        className="font-medium text-foreground underline-offset-4 hover:underline"
      >
        {link.identifier}
      </AppLink>
      {text.slice(at + link.identifier.length)}
    </>
  );
}

// Helpers

// Stable reference for threads with no replies. Inline `[]` would create a
// new array on every render and bust React.memo on CommentCard / ResolvedThreadBar.
const EMPTY_REPLIES: TimelineEntry[] = [];

// Stable initial value for rootIdsWithActiveFocusOrSelection — avoids a
// needless first setState when nothing is focused/selected on mount.
const EMPTY_ID_SET: Set<string> = new Set();

// Sidebar progressive disclosure
//
// Properties shown in the sidebar split into two groups:
//   - core: always rendered (status / assignee / project)
//   - optional: rendered only when the issue has a value for that field OR
//     the user explicitly added it via "+ Add property" in this session
//     (priority / due_date / labels)
//
// Parent is not in either group — the Hierarchy minimap above the Properties
// block shows it (with siblings and children) when the issue has one.
//
// `OPTIONAL_PROP_KEYS` is the open set — adding a new optional field
// means appending here, wiring its row in the JSX switch below, and
// adding a locale key. The picker, visibility rules, and add-property
// menu all flow from this one list.
// `stage` is only meaningful for a sub-issue (relative to its siblings), so
// its row and add-property entry are gated on `issue.parent_issue_id` at the
// render site below — it stays in this list so seeding/visibility flow through
// the same machinery as the other optional props.
/** The sidebar sections a guest gets: read-only facts about the issue itself. */
const GUEST_SIDEBAR_SECTIONS = new Set<IssueSidebarSection>(["properties", "parent", "deliverables"]);

const OPTIONAL_PROP_KEYS = ["project", "priority", "stage", "start_date", "due_date", "labels"] as const;
type OptionalPropKey = (typeof OPTIONAL_PROP_KEYS)[number];

function isOptionalPropSet(
  issue: Issue,
  key: OptionalPropKey,
  attachedLabelsCount: number,
): boolean {
  switch (key) {
    case "project":
      return !!issue.project_id;
    case "priority":
      return issue.priority !== "none";
    case "stage":
      return issue.stage !== null && issue.stage !== undefined;
    case "start_date":
      return !!issue.start_date;
    case "due_date":
      return !!issue.due_date;
    case "labels":
      return attachedLabelsCount > 0;
  }
}

// groupSubIssuesByStage orders a parent's children for display: staged groups
// ascending by stage, then the unstaged group (stage === null) last. Callers
// render a per-group stage header only when the set is actually staged.
export function groupSubIssuesByStage(
  children: Issue[],
): { stage: number | null; items: Issue[] }[] {
  const byStage = new Map<number, Issue[]>();
  const unstaged: Issue[] = [];
  for (const c of children) {
    if (c.stage != null) {
      const arr = byStage.get(c.stage);
      if (arr) arr.push(c);
      else byStage.set(c.stage, [c]);
    } else {
      unstaged.push(c);
    }
  }
  const groups: { stage: number | null; items: Issue[] }[] = [...byStage.keys()]
    .sort((a, b) => a - b)
    .map((s) => ({ stage: s, items: byStage.get(s) as Issue[] }));
  if (unstaged.length > 0) groups.push({ stage: null, items: unstaged });
  return groups;
}

// Shallow array equality by element identity. Used to reuse the previous
// render's per-thread reply slice when nothing in *this* thread changed,
// even if the surrounding `timeline` array was rebuilt by a WS event in
// some unrelated thread.
function shallowEqualEntries(a: TimelineEntry[], b: TimelineEntry[]): boolean {
  if (a === b) return true;
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

// Flat per-item shape consumed by <Virtuoso>. Virtuoso needs a flat array
// where each entry is one rendered row; we keep the grouping logic from
// `timelineView.groups` (consecutive same-actor activities still collapse
// into one activity-group row) but project it into a discriminated union
// the itemContent dispatcher can switch on.
type TimelineItem =
  | { kind: "run"; id: string; run: CommentRun; entry?: TimelineEntry; row?: CommentThreadRow }
  | { kind: "comment"; id: string; entry: TimelineEntry; row?: CommentThreadRow }
  | { kind: "thread-row"; id: string; entry: TimelineEntry; row: CommentThreadRow }
  | { kind: "resolved-bar"; id: string; entry: TimelineEntry }
  | { kind: "activity-group"; id: string; entries: TimelineEntry[] };

function timelineItemKey(item: TimelineItem): string {
  return `${item.kind}:${item.kind === "run" ? item.run.task.id : item.kind === "thread-row" ? item.row.key ?? item.id : item.id}`;
}

type RawTimelineGroup = {
  type: "comment" | "activities";
  entries: TimelineEntry[];
} | { type: "run"; run: CommentRun; entry?: TimelineEntry };

function flattenGroups(
  groups: ReadonlyArray<RawTimelineGroup>,
  expandedResolved: ReadonlySet<string>,
): TimelineItem[] {
  const out: TimelineItem[] = [];
  for (const group of groups) {
    if (group.type === "run") {
      out.push({ kind: "run", id: group.entry?.id ?? group.run.task.id, run: group.run, entry: group.entry });
    } else if (group.type === "comment") {
      const entry = group.entries[0]!;
      const isResolved = !!entry.resolved_at;
      const isExpanded = expandedResolved.has(entry.id);
      out.push(
        isResolved && !isExpanded
          ? { kind: "resolved-bar", id: entry.id, entry }
          : { kind: "comment", id: entry.id, entry },
      );
    } else {
      out.push({
        kind: "activity-group",
        id: group.entries[0]!.id,
        entries: group.entries,
      });
    }
  }
  return out;
}

function ResolvedToggleReveal({
  id,
  domId,
  toggledRef,
  className,
  children,
}: {
  id: string;
  domId?: string;
  toggledRef: React.RefObject<Set<string>>;
  className?: string;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    if (!toggledRef.current.delete(id)) return;
    const anim = revealElement(ref.current, UI_MOTION_DURATION.fast * 1000, -3);
    return () => { anim?.cancel(); };
  }, [id, toggledRef]);
  return <div ref={ref} id={domId} className={className}>{children}</div>;
}

function TimelineSkeleton() {
  return (
    <div className="mt-4 flex flex-col gap-3">
      {[0, 1].map((i) => (
        <div key={i} className="flex gap-3 p-4">
          <Skeleton className="h-10 w-10 shrink-0 rounded-full" />
          <div className="flex-1 space-y-2">
            <Skeleton className="h-4 w-32" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-4/5" />
          </div>
        </div>
      ))}
    </div>
  );
}

// When the trailing block is expanded, we still truncate its body to the most
// recent N entries — a single block of 50 status flips drowns the comment area
// as badly as N blocks of 1 would. Older entries fold behind a "Show N more
// activities" line that expands in place.
// Median comment row height for the timeline's Virtuoso, measured on the
// scroll-stability D1 fixture (45 comments, prose plus one code block; median
// 610 px, range 588-610). Virtuoso sizes rows it has not measured from this,
// so rows mounting above the reader no longer start at the wrong height and
// shift the offset (CHE-3235). Real measurements replace it per row.
const TIMELINE_ROW_ESTIMATED_HEIGHT = 610;

const LAST_ACTIVITY_BLOCK_VISIBLE_LIMIT = 8;

// Collapsible wrapper for an activity block. Older blocks default to a single
// "N activities" summary line so the timeline isn't dominated by status /
// priority / assignee churn; the trailing block stays expanded because it
// usually answers "what just happened?". Expansion state is owned by the
// parent so it survives Virtuoso's mount/unmount on scroll.
function ActivityBlock({
  issueId,
  entries,
  expanded,
  onToggle,
  truncateOlder,
  showOlder,
  onToggleShowOlder,
  getActorName,
  resolveStatusLabel,
  resolveStatusCategory,
  resolveStatusColor,
  resolveStatusIcon,
  t,
  timeAgo,
  locale,
  requesterName,
}: {
  issueId: string;
  entries: TimelineEntry[];
  expanded: boolean;
  onToggle: () => void;
  // Trailing block only: when true, the body shows only the most recent
  // LAST_ACTIVITY_BLOCK_VISIBLE_LIMIT entries with the older ones folded
  // behind a "Show N more activities" inline toggle.
  truncateOlder: boolean;
  showOlder: boolean;
  onToggleShowOlder: () => void;
  getActorName: (type: string, id: string) => string;
  resolveStatusLabel: (statusKey: string) => string;
  resolveStatusCategory: (statusKey: string) => IssueStatusCategory;
  /** A custom status's own `#rrggbb`; null for built-ins and unknown keys. */
  resolveStatusColor: (statusKey: string) => string | null;
  resolveStatusIcon: (statusKey: string) => string | null;
  t: ActivityT;
  timeAgo: (dateStr: string) => string;
  locale: string;
  requesterName?: string;
}) {
  const wakeupText = useWakeupText();
  const hiddenOlderCount =
    expanded && truncateOlder && !showOlder && entries.length > LAST_ACTIVITY_BLOCK_VISIBLE_LIMIT
      ? entries.length - LAST_ACTIVITY_BLOCK_VISIBLE_LIMIT
      : 0;
  const visibleEntries =
    hiddenOlderCount > 0 ? entries.slice(-LAST_ACTIVITY_BLOCK_VISIBLE_LIMIT) : entries;
  // Hide the "v N activities" collapse header while we're in the truncated
  // default state. The "Show N more" link is the only control users need
  // when they're glancing at recent activity — stacking two chevron rows
  // looked like nested folds and added visual noise without value. Once the
  // user explicitly reveals older entries, the header reappears so they can
  // fold the whole block back to a single count line.
  const showHeader = !expanded || hiddenOlderCount === 0;
  return (
    <div className={cn("pb-3 px-4 max-md:px-3", expanded && "flex flex-col gap-3")}>
      {showHeader && (
        <button
          type="button"
          onClick={onToggle}
          className="flex items-center gap-1.5 text-label text-muted-foreground transition-colors hover:text-foreground"
        >
          <ChevronRight
            className={cn(
              "h-3 w-3 shrink-0 transition-transform duration-150 ease-[cubic-bezier(0.23,1,0.32,1)] motion-reduce:transition-none",
              expanded && "rotate-90",
            )}
          />
          <span>{t(($) => $.activity.activity_count, { count: entries.length })}</span>
        </button>
      )}
      {expanded && hiddenOlderCount > 0 && (
        <button
          type="button"
          onClick={onToggleShowOlder}
          className="flex items-center gap-1.5 text-label text-muted-foreground transition-colors hover:text-foreground"
        >
          <ChevronRight className="h-3 w-3 shrink-0" />
          <span>{t(($) => $.activity.show_more_activities, { count: hiddenOlderCount })}</span>
        </button>
      )}
      {expanded && visibleEntries.map((entry) => {
        const details = (entry.details ?? {}) as Record<string, string>;
        // Duplicate rows replace the status rows of the same write, so they
        // carry the status glyph those rows would have had.
        const isStatusChange = entry.action === "status_changed";
        const markedDuplicate = entry.action === "duplicate_marked";
        const unmarkedDuplicate = entry.action === "duplicate_unmarked" && !!details.to;
        const isPriorityChange = entry.action === "priority_changed";
        const isStartDateChange = entry.action === "start_date_changed";
        const isDueDateChange = entry.action === "due_date_changed";

        const isWakeup = WAKEUP_ACTIVITY_ACTIONS.has(entry.action ?? "");
        const chip = isWakeup
          ? wakeupActivityChip(entry, t, wakeupText, getActorName)
          : entry.action === "squad_handoff"
            ? squadHandoffNote(entry, t)
            : null;
        let leadIcon: React.ReactNode;
        if (isWakeup) {
          leadIcon = <WakeupActivityIcon entry={entry} />;
        } else if ((isStatusChange && details.to) || markedDuplicate || unmarkedDuplicate) {
          const to = markedDuplicate ? "cancelled" : details.to;
          leadIcon = (
            <StatusIcon
              status={to as IssueStatus}
              category={resolveStatusCategory(to ?? "")}
              color={resolveStatusColor(to ?? "")}
              icon={resolveStatusIcon(to ?? "")}
              className="h-4 w-4 shrink-0"
            />
          );
        } else if (isPriorityChange && details.to) {
          leadIcon = <PriorityIcon priority={details.to as IssuePriority} className="h-4 w-4 shrink-0" />;
        } else if (isStartDateChange) {
          leadIcon = <CalendarClock className="h-4 w-4 shrink-0 text-muted-foreground" />;
        } else if (isDueDateChange) {
          leadIcon = <Calendar className="h-4 w-4 shrink-0 text-muted-foreground" />;
        } else {
          leadIcon = (
            <ActorAvatar
              actorType={entry.actor_type}
              actorId={entry.actor_id}
              name={entry.actor_name}
              avatarUrl={entry.actor_avatar_url}
              profileRequiresDirectoryEntry
              size="sm"
            />
          );
        }

        const handoff = isStatusChange && !!details.handoff_to_id;
        return (
          <div key={entry.id} className="text-label text-muted-foreground">
            <div className="flex items-center">
              <div className="mr-2 flex w-4 shrink-0 justify-center">{leadIcon}</div>
              <div className={cn("flex min-w-0 flex-1 items-center gap-1", isWakeup && "flex-wrap sm:flex-nowrap")}>
                {/* The platform's own entries read as a sentence without an actor. */}
                {!(isWakeup && entry.actor_type === "system") && (
                  <span className="shrink-0 font-medium">
                    {entry.actor_name || getActorName(entry.actor_type, entry.actor_id)}
                  </span>
                )}
                {/* Wakeup sentences name who was woken; on phones they wrap beside the links rather than truncate. */}
                <span className={cn("min-w-0", isWakeup ? "break-words sm:truncate" : "truncate")} data-testid={isWakeup ? "wakeup-activity-sentence" : undefined}>
                  <ActivityText
                    entry={entry}
                    text={isWakeup
                      ? formatWakeupActivity(entry, t, wakeupText, getActorName)
                      : formatActivity(entry, t, locale, getActorName, resolveStatusLabel, requesterName)}
                  />
                </span>
                {isWakeup && <WakeupActivityLinks entry={entry} issueId={issueId} getActorName={getActorName} />}
                {chip && (
                  <span className="ml-auto inline-flex max-w-48 shrink-0 items-center gap-1 truncate rounded-xs bg-muted px-1.5 py-0.5 text-micro text-muted-foreground" data-testid={isWakeup ? "wakeup-activity-chip" : undefined}>
                    {chip}
                  </span>
                )}
                {(entry.coalesced_count ?? 1) > 1 && entry.action !== "task_completed" && entry.action !== "task_failed" && (
                  <span className="shrink-0 rounded-xs bg-muted px-1.5 py-0.5 text-caption font-medium tabular-nums text-muted-foreground">
                    {t(($) => $.activity.coalesced_badge, { count: entry.coalesced_count ?? 1 })}
                  </span>
                )}
                <Tooltip>
                  <TooltipTrigger render={
                    <span className={cn("shrink-0 cursor-default", !chip && "ml-auto")}>
                      {timeAgo(entry.created_at)}
                    </span>
                  } />
                  <TooltipContent side="top">{new Date(entry.created_at).toLocaleString(locale)}</TooltipContent>
                </Tooltip>
              </div>
            </div>
            {handoff && <div className="ml-6"><TimelineHandoff details={details} /></div>}
          </div>
        );
      })}
    </div>
  );
}

// SubIssueRow — sub-issue list item with inline status & assignee editing

function SubIssueRow({
  child,
  childProgress,
  rowProps,
  customProperties,
}: {
  child: Issue;
  /** The sub-issue's OWN children progress (it can itself be a parent). */
  childProgress?: { done: number; total: number };
  /** User-level display preference: which built-in fields the row shows. */
  rowProps: SubIssueRowProperties;
  /** Workspace custom properties the user opted into showing on rows. */
  customProperties: IssueProperty[];
}) {
  const { t } = useT("issues");
  const locale = useLocale();
  const paths = useWorkspacePaths();
  const updateIssue = useUpdateIssue();
  const { selectedIds, toggle: toggleSelected } = useIssueSurfaceSelection();
  const selected = selectedIds.has(child.id);
  const childStatusCatalog = useIssueStatuses(useWorkspaceId());
  // Category, not key: a custom status in the done/cancelled categories is
  // finished work and has to strike through like any other. (MUL-6243)
  const isDone = issueBehavesAsAny(child, ["done", "closed"]);
  const labels = rowProps.labels ? (child.labels ?? []) : [];
  const customPropsWithValue = customProperties.filter(
    (p) => child.properties?.[p.id] !== undefined,
  );

  const handleUpdate = useCallback(
    (updates: Partial<UpdateIssueRequest>) => {
      updateIssue.mutate(
        {
          id: child.id,
          ...updates,
        },
        {
          onError: (err) =>
            toast.error(
              err instanceof Error && err.message
                ? err.message
                : t(($) => $.detail.update_failed),
            ),
        },
      );
    },
    [child.id, updateIssue, t],
  );

  // AppLink wraps only the title/identifier area. Pickers and checkbox are
  // siblings, so their clicks never navigate — no stopPropagation acrobatics
  // and no risk of the native checkbox / picker triggers being blocked.
  // Right-click anywhere on the row opens the shared issue actions menu
  // (priority, labels, dates, delete, …) — the same menu as list rows.
  return (
    <IssueActionsContextMenu issue={child}>
      <div
        className={cn(
          "flex items-center gap-2.5 px-3 py-2 hover:bg-accent/50 transition-colors group/row",
          selected && "bg-accent/30",
        )}
      >
        {/* Priority ⇄ checkbox slot, mirroring the main list rows: the
            priority icon yields to the selection checkbox on hover/focus.
            Opacity (not display) swap keeps the checkbox keyboard-tabbable. */}
        <div className="relative flex h-4 w-4 shrink-0 items-center justify-center">
          {rowProps.priority && (
            <PriorityIcon
              priority={child.priority}
              className={cn(
                "transition-opacity",
                selected
                  ? "opacity-0"
                  : "group-hover/row:opacity-0 group-focus-within/row:opacity-0",
              )}
            />
          )}
          <input
            type="checkbox"
            checked={selected}
            onChange={() => toggleSelected(child.id)}
            aria-label={t(($) => $.detail.select_sub_issue_aria, {
              identifier: child.identifier,
            })}
            className={cn(
              "absolute inset-0 cursor-pointer accent-primary transition-opacity",
              selected
                ? "opacity-100"
                : "opacity-0 group-hover/row:opacity-100 focus-visible:opacity-100",
            )}
          />
        </div>
        <StatusPicker
          status={child.status}
          onUpdate={handleUpdate}
          projectId={child.project_id}
          workflowId={child.workflow_id}
          issue={child}
          align="start"
          trigger={
            <StatusIcon
              status={child.status}
              category={childStatusCatalog.categoryOf(child.status)}
              color={childStatusCatalog.colorOf(child.status)}
              icon={childStatusCatalog.iconOf(child.status)}
              className="h-[15px] w-[15px] shrink-0"
            />
          }
        />
        <AppLink
          href={paths.issueDetail(issueRouteRef(child))}
          className="flex min-w-0 flex-1 items-center gap-2.5"
        >
          <span className="text-caption text-muted-foreground tabular-nums shrink-0">
            {child.identifier}
          </span>
          <IssueAgentActivityIndicator issueId={child.id} />
          <span className="flex min-w-0 flex-1 items-center gap-1.5">
            <span
              className={cn(
                "text-body truncate",
                isDone
                  ? "text-muted-foreground"
                  : "group-hover/row:text-foreground",
              )}
            >
              {child.title}
            </span>
            {labels.length > 0 && (
              <span className="hidden max-w-[200px] shrink-0 items-center gap-1 overflow-hidden md:inline-flex">
                {labels.slice(0, 2).map((label) => (
                  <LabelChip key={label.id} label={label} />
                ))}
                {labels.length > 2 && (
                  <span className="text-micro text-muted-foreground">
                    +{labels.length - 2}
                  </span>
                )}
              </span>
            )}
            {customPropsWithValue.length > 0 && (
              <span className="hidden max-w-[260px] shrink-0 items-center gap-1 overflow-hidden md:inline-flex">
                {customPropsWithValue.slice(0, 3).map((property) => (
                  <span
                    key={property.id}
                    className="inline-flex max-w-[120px] items-center gap-1 rounded-full bg-muted/60 px-1.5 py-0.5 text-micro text-muted-foreground"
                  >
                    <PropertyIcon property={property} className="size-3 text-micro" />
                    <CustomPropertyValueDisplay
                      property={property}
                      value={child.properties?.[property.id]}
                    />
                  </span>
                ))}
              </span>
            )}
            {rowProps.childProgress && childProgress && childProgress.total > 0 && (
              <span className="inline-flex shrink-0 items-center gap-1 rounded-full bg-muted/60 px-1.5 py-0.5">
                <ProgressRing
                  done={childProgress.done}
                  total={childProgress.total}
                  size={11}
                />
                <span className="text-micro text-muted-foreground tabular-nums font-medium">
                  {childProgress.done}/{childProgress.total}
                </span>
              </span>
            )}
          </span>
        </AppLink>
        {rowProps.dueDate && child.due_date && (
          <DueDatePicker
            dueDate={child.due_date}
            onUpdate={handleUpdate}
            align="end"
            trigger={
              <span
                className={cn(
                  "flex shrink-0 items-center gap-1 text-caption tabular-nums",
                  !isDone && isPastDateOnly(child.due_date)
                    ? "text-destructive"
                    : "text-muted-foreground",
                )}
              >
                <CalendarDays className="size-3" />
                {shortDate(child.due_date, locale)}
              </span>
            }
          />
        )}
        {rowProps.assignee && (
          <AssigneePicker
            assigneeType={child.assignee_type}
            assigneeId={child.assignee_id}
            onUpdate={handleUpdate}
            align="end"
            trigger={
              child.assignee_type && child.assignee_id ? (
                <ActorAvatar
                  actorType={child.assignee_type}
                  actorId={child.assignee_id}
                  size="sm"
                  className="shrink-0"
                />
              ) : (
                <span
                  aria-hidden
                  className="h-5 w-5 rounded-full border border-dashed border-muted-foreground/30 shrink-0"
                />
              )
            }
          />
        )}
      </div>
    </IssueActionsContextMenu>
  );
}

// SubIssueDisplayPopover — per-user "which properties do rows show" control

// Reuses the main Display panel's `card_*` labels — the property names are
// identical, so the sub-issues control needs no locale keys of its own.
const SUB_ISSUE_ROW_PROPERTY_LABEL_KEY: Record<
  SubIssueRowPropertyKey,
  "card_priority" | "card_labels" | "card_child_progress" | "card_due_date" | "card_assignee"
> = {
  priority: "card_priority",
  labels: "card_labels",
  childProgress: "card_child_progress",
  dueDate: "card_due_date",
  assignee: "card_assignee",
};

function SubIssueDisplayPopover({
  workspaceProperties,
}: {
  workspaceProperties: IssueProperty[];
}) {
  const { t } = useT("issues");
  const rowProperties = useSubIssueDisplayStore((s) => s.rowProperties);
  const rowPropertyIds = useSubIssueDisplayStore((s) => s.rowPropertyIds);
  const toggleRowProperty = useSubIssueDisplayStore((s) => s.toggleRowProperty);
  const toggleRowPropertyId = useSubIssueDisplayStore((s) => s.toggleRowPropertyId);

  return (
    <Popover>
      <Tooltip>
        <PopoverTrigger
          render={
            <TooltipTrigger
              render={
                <button
                  type="button"
                  className="inline-flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground transition-colors"
                  aria-label={t(($) => $.display.tooltip)}
                >
                  <SlidersHorizontal className="h-3.5 w-3.5" />
                </button>
              }
            />
          }
        />
        <TooltipContent side="bottom">{t(($) => $.display.tooltip)}</TooltipContent>
      </Tooltip>
      <PopoverContent align="end" className="w-56 p-0">
        <div className="px-3 py-2.5">
          <div className="space-y-2">
            {SUB_ISSUE_ROW_PROPERTY_KEYS.map((key) => (
              <label
                key={key}
                className="flex cursor-pointer items-center justify-between"
              >
                <span className="text-body">
                  {t(($) => $.display[SUB_ISSUE_ROW_PROPERTY_LABEL_KEY[key]])}
                </span>
                <Switch
                  size="sm"
                  checked={rowProperties[key]}
                  onCheckedChange={() => toggleRowProperty(key)}
                />
              </label>
            ))}
            {workspaceProperties.map((property) => (
              <label
                key={property.id}
                className="flex cursor-pointer items-center justify-between gap-3"
              >
                <span className="flex min-w-0 items-center gap-1.5 truncate text-body">
                  <PropertyIcon property={property} className="size-3.5 text-caption" />
                  <span className="truncate">{property.name}</span>
                </span>
                <Switch
                  size="sm"
                  checked={rowPropertyIds.includes(property.id)}
                  onCheckedChange={() => toggleRowPropertyId(property.id)}
                />
              </label>
            ))}
          </div>
        </div>
      </PopoverContent>
    </Popover>
  );
}

// Props

/** The Inbox page, hosting this detail for the notification it has open. */
export interface IssueDetailInboxHost {
  /**
   * The notification's group, floating over the end of the timeline, given
   * the issue's own actions to keep in it.
   */
  bar: (issueActions: ReactNode) => ReactNode;
  /** The open notification, so a cold load shows its identifier at once. */
  item?: InboxItem;
}

interface IssueDetailProps {
  issueId: string;
  onDelete?: () => void;
  /**
   * Set on the Inbox page: the breadcrumb is the issue alone (the Inbox's
   * list is beside it, or a phone's back control names it), the
   * notification's group floats above the composer, and the header offers
   * the issue's own page.
   */
  inbox?: IssueDetailInboxHost;
  defaultSidebarOpen?: boolean;
  layoutId?: string;
  /** When set, the issue detail will auto-scroll to this comment and briefly highlight it. */
  highlightCommentId?: string;
  /**
   * Bump to replay the `highlightCommentId` landing on an already-mounted
   * detail. A remount replays it by itself (fresh mount, cleared memento
   * entry); this token is for a host that asks again with neither remount
   * nor guaranteed re-render. The bump both re-arms the landing guard and
   * forces the render that re-reads the cleared memento entry.
   */
  highlightRequestToken?: number;
  /**
   * The host asked for this landing and has not seen it land (a notification
   * clicked on another page). It runs even where this route records the
   * comment as already landed; `onHighlightLanded` reports when it did.
   */
  highlightRequestPending?: boolean;
  onHighlightLanded?: () => void;
  /** The app confirmation an Inbox notification points at; its card scrolls
   * into view and flashes, replayed by `highlightRequestToken`. */
  highlightAppRequestId?: string;
  /**
   * The cached issue is the detail endpoint's own response, read a moment ago
   * by the route, so mounting skips reading it again.
   */
  authoritative?: boolean;
  /**
   * Far-left header slot, replacing the mobile sidebar trigger. A host that
   * embeds this detail one level deep (the inbox, on a phone) passes its own
   * "back" control here instead of stacking a second 48px bar above us — the
   * breadcrumb this header already renders names the issue's container, not
   * the surface the reader arrived from, so only the host can spell that trip.
   */
  leadingAction?: ReactNode;
  /**
   * `"peek"` renders the detail inside the board's side peek (`IssuePeekHost`):
   * a single column with the core properties as pills under the title, no
   * properties sidebar, and `trailingActions` where the sidebar toggle sits.
   */
  variant?: "page" | "peek";
  /** Peek only: host controls at the end of the header (open full page, close). */
  trailingActions?: ReactNode;
  /**
   * Read the viewer's inbox notifications on this issue while it is on screen
   * (MUL-8588). The inbox turns this off for the issue it has open: it reads
   * the notification it selected itself, and keeps one marked unread there.
   */
  readsInbox?: boolean;
  /** The route already completed the authoritative detail read this mount. */
  skipInitialRefetch?: boolean;
  /**
   * Route only: the file name of a `/deliverables/<name>` link. Opens the
   * newest upload of that deliverable once the timeline has loaded.
   */
  openDeliverable?: string;
  /**
   * Route only: the deliverable link has been dealt with (the viewer it
   * opened closed, or the file is missing); the route puts the plain issue
   * address back.
   */
  onDeliverableLinkDone?: () => void;
}

/**
 * The header row a loading or failed detail shows where the breadcrumb goes,
 * at the same height, so the page does not jump when the issue lands.
 */
function DetailStateBar({ children, trailing }: { children?: ReactNode; trailing?: ReactNode }) {
  const titleBar = usePageHeaderTitleBar();
  return (
    <div
      className={cn("flex h-12 shrink-0 items-center gap-2 border-b", PAGE_GUTTER, titleBar.className)}
      style={titleBar.style}
    >
      {children}
      {trailing && <div className="ml-auto flex shrink-0 items-center gap-1">{trailing}</div>}
    </div>
  );
}

// Not found

/**
 * Exported so `IssueDetailRoute` can render it for an identifier that names no
 * issue. The route must NOT fall through to `IssueDetail` in that case: doing
 * so mounts a second observer on the already-failed query, which refetches it
 * and restarts the resolve/remount cycle indefinitely.
 */
export function IssueNotFound({
  showBackLink = true,
  leading,
  trailing,
}: {
  showBackLink?: boolean;
  /**
   * Host-supplied way back, mirrored from `IssueDetailProps.leadingAction`. A
   * host that hands its whole screen to this component (the inbox, on a phone)
   * has no bar of its own left, so this state has to carry the trip back or
   * there is none.
   */
  leading?: ReactNode;
  /** Host controls at the end of the bar — the side peek's close button. */
  trailing?: ReactNode;
}) {
  const { t } = useT("issues");
  const backOrReplace = useBackOrReplace();
  const paths = useWorkspacePaths();
  const titleBar = usePageHeaderTitleBar();

  return (
    <div className="flex flex-1 min-h-0 flex-col">
      {(leading || trailing || titleBar.active) && (
        <DetailStateBar trailing={trailing}>{leading}</DetailStateBar>
      )}
      <CollectionPageState
        icon={FileQuestion}
        title={t(($) => $.detail.not_found_title)}
        description={t(($) => $.detail.not_found)}
        actions={
          showBackLink ? (
            <Button variant="outline" size="sm" onClick={() => backOrReplace(paths.issues())}>
              <ChevronLeft aria-hidden="true" />
              {t(($) => $.detail.back)}
            </Button>
          ) : undefined
        }
      />
    </div>
  );
}

/** Safe primary-read failure; host navigation remains available during retry. */
export function IssueLoadError({
  showBackLink = true,
  leading,
  trailing,
  onRetry,
  isRetrying,
}: {
  showBackLink?: boolean;
  leading?: ReactNode;
  trailing?: ReactNode;
  onRetry: () => Promise<void>;
  isRetrying: boolean;
}) {
  const { t } = useT("issues");
  const backOrReplace = useBackOrReplace();
  const paths = useWorkspacePaths();
  const titleBar = usePageHeaderTitleBar();

  return (
    <div className="flex flex-1 min-h-0 flex-col">
      {(leading || trailing || titleBar.active) && (
        <DetailStateBar trailing={trailing}>{leading}</DetailStateBar>
      )}
      <CollectionPageState
        icon={AlertTriangle}
        title={t(($) => $.detail.load_failed)}
        actions={
          <>
            <Button size="sm" disabled={isRetrying} aria-busy={isRetrying} onClick={() => { void onRetry(); }}>
              {isRetrying && <Loader2 aria-hidden="true" className="animate-spin" />}
              {t(($) => $.detail.load_retry)}
            </Button>
            {showBackLink && (
              <Button variant="outline" size="sm" onClick={() => backOrReplace(paths.issues())}>
                <ChevronLeft aria-hidden="true" />
                {t(($) => $.detail.back)}
              </Button>
            )}
          </>
        }
      />
    </div>
  );
}

// Loading skeleton

const ISSUE_DETAIL_LAYOUT_ID = "multica_issue_detail_layout";
const ISSUE_PAGE_TITLE_CLASS = "text-display-sm font-semibold";
/**
 * Exported so `IssueDetailRoute` can show the identical frame while it resolves
 * an identifier URL to its issue — the two waits are indistinguishable to the
 * user, and rendering the same skeleton keeps them that way.
 */
function IssueSidebarSkeletonRows({ className }: { className?: string }) {
  return (
    <div className={cn("space-y-5", className)}>
      {Array.from({ length: 4 }).map((_, i) => (
        <div key={i} className="flex items-center gap-2">
          <Skeleton className="h-3 w-16 shrink-0" />
          <Skeleton className="h-5 w-24" />
        </div>
      ))}
      <Skeleton className="h-px w-full" />
      {Array.from({ length: 3 }).map((_, i) => (
        <div key={i} className="flex items-center gap-2">
          <Skeleton className="h-3 w-16 shrink-0" />
          <Skeleton className="h-4 w-28" />
        </div>
      ))}
    </div>
  );
}

export function IssueDetailSkeleton({
  leading,
  trailing,
  sidebar = true,
  layoutId = ISSUE_DETAIL_LAYOUT_ID,
  inboxItem,
}: {
  leading?: ReactNode;
  trailing?: ReactNode;
  sidebar?: boolean;
  layoutId?: string;
  inboxItem?: InboxItem | null;
} = {}) {
  const { defaultLayout } = useDefaultLayout({ id: layoutId });
  const sidebarOpen = sidebar && getAnimatedRightSidebarInitialOpen(true, defaultLayout);

  return (
    <div className="flex flex-1 min-h-0 flex-col">
      {/* The way back is real from the first frame, not once the issue lands:
          a host that gave up its own bar for `leadingAction` has nothing else
          to offer while this skeleton owns the screen. */}
      <DetailStateBar trailing={trailing}>
        {leading ?? (
          inboxItem?.issue_identifier ? (
            <span className="text-caption font-medium text-muted-foreground">
              {inboxItem.issue_identifier}
            </span>
          ) : (
            <>
              <Skeleton className="h-4 w-16" />
              <Skeleton className="h-4 w-4" />
              <Skeleton className="h-4 w-24" />
            </>
          )
        )}
      </DetailStateBar>
      <div className="flex flex-1 min-h-0">
        {/* Same scrollbar-gutter as the loaded scroller below, so the skeleton
            column doesn't shift sideways when real content mounts. */}
        <div className="@container flex-1 overflow-y-auto [scrollbar-gutter:stable_both-edges]">
          {/* Width and gutters match the loaded column exactly (see its
              comment), so the skeleton doesn't reflow sideways when real
              content mounts. */}
          <div className={cn(READING_COLUMN, "px-[var(--space-sm,0.75rem)] py-[var(--space-lg,1.5rem)] space-y-6 md:px-[var(--space-xl,2rem)] md:py-[var(--space-xl,2rem)]")}>
            {inboxItem?.title ? (
              <div className={cn("w-full leading-snug text-foreground", ISSUE_PAGE_TITLE_CLASS)}>
                {inboxItem.title}
              </div>
            ) : (
              <Skeleton className="h-8 w-3/4" />
            )}
            <div className="space-y-2">
              <Skeleton className="h-4 w-full" />
              <Skeleton className="h-4 w-5/6" />
              <Skeleton className="h-4 w-2/3" />
            </div>
            <Skeleton className="h-px w-full" />
            <div className="space-y-3">
              <Skeleton className="h-4 w-20" />
              <div className="flex items-start gap-3">
                <Skeleton className="h-8 w-8 shrink-0 rounded-full" />
                <div className="flex-1 space-y-2">
                  <Skeleton className="h-4 w-32" />
                  <Skeleton className="h-16 w-full rounded-lg" />
                </div>
              </div>
            </div>
          </div>
        </div>
        {sidebarOpen && (
          <div className="hidden md:block w-80 border-l p-4">
            <IssueSidebarSkeletonRows />
          </div>
        )}
      </div>
    </div>
  );
}

// IssueDetail

/** Upper bound on a deep link holding the sidebar, composer and thread rail back. */
const LANDING_CHROME_HOLD_MAX_MS = 500;

export function IssueDetail({ issueId, onDelete, inbox, defaultSidebarOpen = true, layoutId = ISSUE_DETAIL_LAYOUT_ID, highlightCommentId: linkedCommentId, highlightRequestToken: linkedRequestToken, highlightRequestPending = false, onHighlightLanded, highlightAppRequestId, authoritative, leadingAction, variant = "page", trailingActions, readsInbox = true, skipInitialRefetch = false, openDeliverable, onDeliverableLinkDone }: IssueDetailProps) {
  const isPeek = variant === "peek";
  const { t } = useT("issues");
  const { t: tCommon } = useT("common");
  const locale = useLocale();
  const timeAgo = useTimeAgo();
  const id = issueId;
  const [deliverableJump, setDeliverableJump] = useState<{
    issueId: string;
    linkedCommentId: string | undefined;
    linkedRequestToken: number | undefined;
    commentId: string;
    token: number;
  } | null>(null);
  const activeDeliverableJump = deliverableJump?.issueId === id &&
    deliverableJump.linkedCommentId === linkedCommentId &&
    deliverableJump.linkedRequestToken === linkedRequestToken ? deliverableJump : null;
  const highlightCommentId = activeDeliverableJump?.commentId ?? linkedCommentId;
  const highlightRequestToken = activeDeliverableJump?.token ?? linkedRequestToken;
  useEffect(() => {
    setDeliverableJump(null);
  }, [id, linkedCommentId, linkedRequestToken]);
  const user = useAuthStore((s) => s.user);
  const paths = useWorkspacePaths();
  const openModal = useModalStore((state) => state.open);

  // Issue navigation — read from TQ list cache
  const wsId = useWorkspaceId();
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  // A guest (MUL-9420) reads and comments; it edits nothing and sees no runs.
  const guest = useIsGuest(wsId);
  // Workspace owners and admins moderate any comment authored by anyone
  // (mirrors backend `comment.go:507-512`). Computed here so per-comment
  // rendering doesn't have to re-derive it for every row.
  const currentUserRole =
    members.find((m) => m.user_id === user?.id)?.role ?? null;
  const canModerateComments =
    currentUserRole === "owner" || currentUserRole === "admin";
  // Cache read only: the list seeds the row another view already loaded, and
  // is never worth its own workspace-wide request.
  const { data: allIssues = [] } = useQuery({ ...issueListOptions(wsId), enabled: false });
  const { getActorName } = useActorName();
  const resolveStatusLabel = useStatusLabel(wsId);
  // Activity and issue visuals share the catalog's custom geometry and color;
  // built-ins keep their fixed glyph and semantic token.
  const { categoryOf: resolveStatusCategory, colorOf: resolveStatusColor, iconOf: resolveStatusIcon } =
    useIssueStatuses(wsId);
  // Description autosave is deliberately NOT gated (no explicit submit; the
  // editor already strips `blob:` before serializing and binds ids on the
  // later save). It still needs the failure toast, or a failed upload just
  // erases its own placeholder and the file disappears unexplained.
  const {
    uploadWithToast,
    storageRefusal: descStorageRefusal,
    dismissStorageRefusal: dismissDescStorageRefusal,
  } = useEditorUpload({ inlineStorageNotice: true });
  const { defaultLayout, onLayoutChanged } = useDefaultLayout({
    id: layoutId,
  });
  const sidebarRef = usePanelRef();
  const isMobile = useIsMobile();
  const desktopSidebarInitialOpen = getAnimatedRightSidebarInitialOpen(
    defaultSidebarOpen,
    defaultLayout,
  );
  const {
    open: desktopSidebarOpen,
    visualOpen: desktopSidebarVisualOpen,
    motionEnabled: desktopSidebarMotionEnabled,
    beginToggle: beginDesktopSidebarToggle,
    handleResize: handleDesktopSidebarResize,
  } = useAnimatedRightSidebarState(desktopSidebarInitialOpen);
  const [mobileSidebarOpen, setMobileSidebarOpen] = useState(false);

  useEffect(() => {
    if (isMobile) {
      setMobileSidebarOpen(false);
    }
  }, [isMobile]);
  const sidebarOpen = isMobile ? mobileSidebarOpen : desktopSidebarOpen;
  const [propertiesOpen, setPropertiesOpen] = useState(true);
  const [parentIssueOpen, setParentIssueOpen] = useState(true);
  // An empty section the reader picked from the add row, shown until its form closes.
  const [sidebarReveal, setSidebarReveal] = useState<IssueSidebarAddable | null>(null);
  useEffect(() => setSidebarReveal(null), [id]);
  const [pullRequestsOpen, setPullRequestsOpen] = useState(true);
  const githubSettings = useGitHubSettings();
  const pluginsEnabled = useFeatureEnabled(PLUGINS_V1_FLAG, false);

  // Per-issue, per-session set of optional properties currently visible in
  // the sidebar Properties section. Seeded on issue switch with whichever
  // fields are already set; "+ Add property" adds an entry, clearing a
  // value does *not* remove one (avoids row-flicker on edit → clear).
  // Resets when the user navigates to a different issue.
  const [visibleOptionalProps, setVisibleOptionalProps] = useState<Set<OptionalPropKey>>(
    () => new Set(),
  );
  // Same progressive-disclosure machinery for custom properties, keyed by
  // property definition id instead of a static key union.
  const [visibleCustomProps, setVisibleCustomProps] = useState<Set<string>>(() => new Set());
  const [autoOpenCustomProp, setAutoOpenCustomProp] = useState<string | null>(null);
  // Optional property to auto-open as soon as it's mounted (the user just
  // picked it from "+ Add property" and we want them dropped straight into
  // edit state). Consumed by the row that matches this key, cleared after.
  const [autoOpenProp, setAutoOpenProp] = useState<OptionalPropKey | null>(null);
  // Controlled state for the "+ Add property" popover. Base UI's Popover
  // doesn't auto-dismiss on item click (it's not a Menu primitive), so the
  // popover would stay open behind the newly auto-opened picker — two
  // popovers stacked. We close it explicitly in `addOptionalProp`.
  const [addPropPopoverOpen, setAddPropPopoverOpen] = useState(false);
  // Virtuoso's `customScrollParent` wants the HTMLElement, not a ref. A plain
  // `useRef.current` does not trigger a re-render when it populates, so the
  // Virtuoso prop would never receive the element. Callback ref + state fixes
  // that: setState triggers the re-render that hands Virtuoso the element.
  const [scrollContainerEl, setScrollContainerEl] = useState<HTMLDivElement | null>(null);
  // Bottom comment composer. Measured when scrolling a freshly posted comment
  // into view so its bottom lands above the composer rather than behind it.
  const composerRef = useRef<HTMLDivElement | null>(null);
  // Pull-based scroll restoration (MUL-4741): the platform serves the offset
  // captured when this route was last left. The ref-attach assignment covers
  // content that is already there; useIssueDetailScrollRestore applies it
  // again once the timeline has loaded. The timeline's Virtuoso must not get
  // it as `initialScrollTop`: with `customScrollParent` Virtuoso counts that
  // from the list's top, which sits below the description, so the page came
  // back a description's height further down.
  //
  // The container key carries the issue id: on the issue route that is
  // redundant with the route scoping, but in the inbox the selection lives
  // in the query string while memento entries key by pathname — a bare
  // "main" would restore notification A's offset into notification B's
  // detail.
  // The peek gets its own key: it is an overlay on the board's route, and
  // must neither restore the full page's offset nor leave one behind for it.
  const scrollContainerKey = `${isPeek ? "peek" : "main"}:${id}`;
  const restoredScrollTop = useRestoredScrollOffset(scrollContainerKey);
  const restoreScrollRef = useRestoredScrollRef(scrollContainerKey);
  // Whether this issue's comment-highlight deep link already landed here
  // (survives the tab's unmount, unlike didHighlightRef below): a remount
  // that restores the view must not re-run the jump.
  //
  // The landing effect reads it through a ref, NOT through its dependency
  // array: recording the landing writes this very value, and as an effect
  // dependency that write would re-fire the effect one commit after the
  // jump — whose cleanup cancels the centering rAF loop and the highlight
  // fade before a single frame ran. Every path that must re-evaluate the
  // landing already re-runs the effect through another dependency (mount,
  // items arriving, token bump), and the render that precedes it refreshes
  // this ref.
  //
  // A pending request ignores the entry: the host has not seen it land.
  const consumedHighlightId = useRestoredViewState(issueHighlightMementoKey(id));
  const highlightConsumed =
    !!highlightCommentId && !highlightRequestPending && consumedHighlightId === highlightCommentId;
  const highlightConsumedRef = useRef(highlightConsumed);
  highlightConsumedRef.current = highlightConsumed;
  // The comment id the list was OPENED at (initialTopMostItemIndex), set once when
  // the list first renders for this issue. While it matches the landing target,
  // Virtuoso owns the scroll and the landing effect must not write to it: a
  // scrollTop write mid-placement makes Virtuoso scroll to NaN, which snaps the
  // page to the top (CHE-5376).
  const listOpenedAtRef = useRef<{ listKey: string; commentId: string | null } | null>(null);
  const reduceMotion = useReducedMotion();
  const pendingInboxTransition = useInboxSelectionStore((s) =>
    wsId ? s.transitionByWorkspace[wsId] : undefined,
  );
  const [inboxTransition, setInboxTransition] = useState<{
    issueId: string;
    direction: InboxTransitionDirection;
  } | null>(null);
  const activeInboxTransition: InboxTransitionDirection | null = isPeek
    ? null
    : inboxTransition?.issueId === id
      ? inboxTransition.direction
      : pendingInboxTransition?.targetKey === id
        ? pendingInboxTransition.direction
        : null;
  const onHighlightLandedRef = useRef(onHighlightLanded);
  onHighlightLandedRef.current = onHighlightLanded;
  const writeViewState = useViewStateWriter();
  const rightSidebarShortcutTargetRef = useRef<HTMLDivElement | null>(null);
  const attachScrollContainer = useCallback(
    (el: HTMLDivElement | null) => {
      // The peek has no sidebar, so it must not claim the toggle shortcut.
      rightSidebarShortcutTargetRef.current = isPeek ? null : el;
      setScrollContainerEl(el);
      restoreScrollRef(el);
    },
    [isPeek, restoreScrollRef],
  );
  const [pendingPostedCommentId, setPendingPostedCommentId] = useState<string | null>(null);
  const scrollToTimelineBottom = useCallback((commentId: string) => {
    setPendingPostedCommentId(commentId);
  }, []);
  const [highlightedId, setHighlightedId] = useState<string | null>(null);
  // User preference: pin the bottom comment bar to the scroll viewport. Off
  // below `md` regardless of the preference — see the hook.
  const stickyComposer = useStickyComposer();
  // Read by the image rules in editor/styles/attachment.css; under
  // "thumbnails" the timeline's images also load the server's thumbnails.
  const commentImageLayout = useCommentComposerStore((s) => s.imageLayout);

  // Per-session: which resolved threads the user has temporarily expanded.
  // Not persisted (matches Linear) — reload collapses everything back to bars.
  // Store-backed (not component state) so the command palette's fold/unfold
  // all-comments commands can drive it from outside this page.
  const expandedResolved = useResolvedExpandStore(selectExpandedResolved(id));
  const setResolvedExpanded = useResolvedExpandStore((s) => s.setExpanded);
  // One-shot WAAPI reveal for user-clicked resolved thread expand/collapse only
  // (Virtuoso scroll remounts never read a populated id).
  const recentResolvedToggleRef = useRef<Set<string>>(new Set());
  const toggleResolvedExpand = useCallback((commentId: string, expand: boolean) => {
    recentResolvedToggleRef.current.add(commentId);
    setResolvedExpanded(id, commentId, expand);
    // On collapse the thread shrinks and the viewport would jump to whatever was
    // below; pull the just-folded thread back into view with the smallest
    // movement. rAF waits for the collapse to land before measuring.
    if (!expand) {
      requestAnimationFrame(() =>
        document.getElementById(`comment-${commentId}`)?.scrollIntoView({ block: "nearest" }),
      );
    }
  }, [id, setResolvedExpanded]);
  const clearResolvedExpand = useCallback((commentId: string) => {
    setResolvedExpanded(id, commentId, false);
  }, [id, setResolvedExpanded]);

  // Per-session, per-root "all replies" length preference (01-DESIGN "Durable
  // state and transition matrix") — unresolved threads over three replies
  // default to compact (latest three); Show more/less remembers the choice
  // for the rest of the session and survives row unmount/remount because it
  // lives in this store, not row-local useState. Also driven from outside
  // this page by the command palette's fold/unfold-all-comments commands.
  const expandedThreadLengths = useIssueDisclosureStore(selectExpandedThreads(id));
  const setThreadExpanded = useIssueDisclosureStore((s) => s.setThreadExpanded);
  const toggleThreadLengthExpand = useCallback((rootId: string, expand: boolean) => {
    setThreadExpanded(id, rootId, expand);
  }, [id, setThreadExpanded]);
  // Roots Fold All just cleared from `expandedThreadLengths`, read by the
  // latch effect below to distinguish "just fold-all'd, still mid-tick" from
  // "never expanded" (CHE-479) — see issue-disclosure-store.ts's field doc.
  // `justFoldedRoots` is a NEW Set instance every time `collapseAllThreads`
  // runs (it is never mutated in place), so its object identity itself is
  // the "which fold occurrence" token — the latch effect below tracks which
  // Set instance it has already responded to per root, rather than clearing
  // the store's copy, so this component never needs a second render just to
  // consume/reset the signal.
  const justFoldedRoots = useIssueDisclosureStore(selectJustFoldedRoots(id));
  // The `justFoldedRoots` instance the latch effect below last saw — reset
  // trigger for the two refs beneath it: a NEW instance (a distinct Fold All
  // occurrence) means both start over from scratch, with no memory of what
  // an earlier occurrence suppressed or voided.
  const lastSeenJustFoldedRootsRef = useRef<ReadonlySet<string>>(EMPTY_ID_SET);
  // Root ids still suppressed (skipped by the latch) under the CURRENT
  // `justFoldedRoots` occurrence. Seeded from `justFoldedRoots` itself when
  // that occurrence starts; shrinks only via `voidedFoldRootsRef` below.
  const suppressedFoldRootsRef = useRef<Set<string>>(new Set());
  // Root ids whose suppression has been permanently released for the
  // CURRENT `justFoldedRoots` occurrence because their own latch reason
  // stopped being active at least once since that occurrence started — this
  // is what lets a reply draft that is cleared and then restarted on the
  // same root latch normally again, without waiting for a distinct new Fold
  // All to reset `suppressedFoldRootsRef` wholesale.
  const voidedFoldRootsRef = useRef<Set<string>>(new Set());

  // Per-session activity-block expansion overrides. The default rule is
  // "only the trailing block is expanded" (computed from timelineView.groups
  // below); these two sets capture user clicks that diverge from the default.
  // Two sets are needed because "default" can flip when a new activity block
  // appends — without an explicit collapse override, a manually-collapsed
  // older block would re-expand when it stops being the trailing one (or vice
  // versa). Not persisted, matches the resolved-thread behaviour above.
  const [expandedActivityIds, setExpandedActivityIds] = useState<Set<string>>(() => new Set());
  const [collapsedActivityIds, setCollapsedActivityIds] = useState<Set<string>>(() => new Set());
  // Block IDs where the user has explicitly chosen to also reveal the older
  // (pre-last-8) entries within the trailing block. Kept independent of the
  // expanded/collapsed sets so collapsing then re-expanding preserves the
  // "show all" choice, and so the choice survives the block losing its
  // trailing position when a new comment lands after it.
  const [showOlderActivityIds, setShowOlderActivityIds] = useState<Set<string>>(() => new Set());
  const toggleActivityBlock = useCallback((id: string, currentlyExpanded: boolean) => {
    if (currentlyExpanded) {
      setCollapsedActivityIds((prev) => {
        const next = new Set(prev);
        next.add(id);
        return next;
      });
      setExpandedActivityIds((prev) => {
        if (!prev.has(id)) return prev;
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
    } else {
      setExpandedActivityIds((prev) => {
        const next = new Set(prev);
        next.add(id);
        return next;
      });
      setCollapsedActivityIds((prev) => {
        if (!prev.has(id)) return prev;
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
    }
  }, []);
  const showOlderActivities = useCallback((id: string) => {
    setShowOlderActivityIds((prev) => {
      if (prev.has(id)) return prev;
      const next = new Set(prev);
      next.add(id);
      return next;
    });
  }, []);
  const didHighlightRef = useRef<string | null>(null);
  // Highlight whose landing was cancelled after its anchor mounted but before
  // it finished aligning; the landing effect resumes it.
  const interruptedLandingRef = useRef<string | null>(null);
  // Last seen highlightRequestToken; a bump re-arms didHighlightRef so the
  // landing effect below replays on an already-mounted detail.
  const lastHighlightRequestTokenRef = useRef(highlightRequestToken);

  // Issue data from TQ — uses detail query, seeded from list cache if available.
  // Only seed when description is present; the list API omits it, so a
  // partial list row must not masquerade as a hydrated issue detail.
  // The view's own observer reconciles with the authoritative detail endpoint
  // on mount unless the route already completed that read (`authoritative`
  // from the fork route, `skipInitialRefetch` from the upstream contract).
  const issueQuery = useQuery(
    issueDetailViewOptions(wsId, id, allIssues.find((i) => i.id === id), authoritative || skipInitialRefetch),
  );
  const { data: issue = null, isLoading: issueLoading, refetch: refetchIssue } = issueQuery;
  const issueReadState = useIssueReadState(wsId, id, issueQuery);
  useMarkIssueReadWhileViewing(wsId, issue?.id ?? "");
  useMarkIssueInboxReadWhileViewing(wsId, issue?.id ?? "", readsInbox);
  const descriptionSourceId = `description:${id}`;
  const descriptionAnnotations = useCommentAnnotations({
    issueId: id,
    draftKey: `new:${id}`,
    sources: [{ id: descriptionSourceId, name: "Description", revision: issue?.revision }],
    enabled: !!user && !!issue,
    editable: true,
  });
  const canAnnotateDescription = !!user;
  const descriptionSelectionAction = useMemo(() => canAnnotateDescription ? {
    label: t(($) => $.reply.annotations.add_comment),
    onSelect: descriptionAnnotations.addSelection,
  } : undefined, [canAnnotateDescription, t, descriptionAnnotations.addSelection]);
  const openCommentSubIssue = useCallback((commentId: string, prompt?: string) => {
    if (!issue) return;
    openModal("quick-create-issue", {
      ...subIssueCreateDefaults(issue),
      anchor_comment_id: commentId,
      ...(prompt ? { prompt } : {}),
    });
  }, [issue, openModal]);

  // Record recent visit
  const recordVisit = useRecentIssuesStore((s) => s.recordVisit);
  const recordRecentContext = useRecentContextStore((s) => s.recordVisit);
  useEffect(() => {
    if (issue) {
      recordVisit(wsId, issue.id);
      recordRecentContext(wsId, {
        type: "issue",
        id: issue.id,
        label: issue.identifier,
        subtitle: issue.title,
        status: issue.status,
      });
    }
  }, [issue?.id, wsId]); // eslint-disable-line react-hooks/exhaustive-deps

  const deletedFallbackPath = onDelete ? undefined : paths.issues();
  useRegisterCommandBarContext(
    useMemo(
      () =>
        issue ? { kind: "issue", issue, onDeletedFallbackPath: deletedFallbackPath } : null,
      [issue, deletedFallbackPath],
    ),
  );

  // Fire `onDelete` once when the issue transitions from loaded to missing.
  // Delete goes through a shell-level modal, so the caller (e.g. inbox) can't
  // be notified directly — instead, the detail page observes its own cache
  // clearing and runs the callback. We navigate via `onDeletedFallbackPath` on
  // the actions menu when no callback is supplied (standalone routes).
  const hadIssueRef = useRef(false);
  const firedDeleteCallbackRef = useRef(false);
  useEffect(() => {
    if (issue) {
      hadIssueRef.current = true;
      firedDeleteCallbackRef.current = false;
      return;
    }
    if (
      hadIssueRef.current &&
      !issueLoading &&
      !firedDeleteCallbackRef.current &&
      onDelete
    ) {
      firedDeleteCallbackRef.current = true;
      onDelete();
    }
  }, [issue, issueLoading, onDelete]);

  // Custom hooks — encapsulate timeline, reactions, subscribers
  const {
    timeline, loading: timelineLoading, dataUpdatedAt: timelineUpdatedAt,
    submitComment, submitReply,
    editComment, deleteComment, toggleResolveComment, toggleReaction: handleToggleReaction,
  } = useIssueTimeline(id, user?.id, activeDeliverableJump?.commentId);

  const { data: commentTasks, isLoading: commentTasksLoading } = useQuery({ ...issueTasksOptions(id), enabled: !guest });

  // A deep link's landing frame leaves out what is off the reading path: the
  // properties sidebar, the composer and the thread rail. They mount once the
  // target has painted, a frame or two after it (MUL-9538). An Inbox click
  // marks its landing pending before the page mounts; the comment it names
  // arrives a render later on web, with the URL's fragment.
  const [landingChromeHeld, setLandingChromeHeld] = useState(
    () => highlightRequestPending || (!!highlightCommentId && !highlightConsumed),
  );
  const releaseLandingChrome = useCallback(() => setLandingChromeHeld(false), []);
  // A landing request whose link names no comment has nothing to land on; the
  // fragment is in by the next frame, so a hold still without a target then is let go.
  const holdingWithoutTarget = landingChromeHeld && !highlightCommentId;
  useEffect(() => {
    if (!holdingWithoutTarget) return;
    const frame = requestAnimationFrame(releaseLandingChrome);
    return () => cancelAnimationFrame(frame);
  }, [holdingWithoutTarget, releaseLandingChrome]);
  // Bounded in case the landing never runs (a target that is gone).
  useEffect(() => {
    if (!landingChromeHeld) return;
    const timer = window.setTimeout(releaseLandingChrome, LANDING_CHROME_HOLD_MAX_MS);
    return () => window.clearTimeout(timer);
  }, [landingChromeHeld, releaseLandingChrome]);
  // Only a landing on data already in hand is worth holding the chrome for;
  // while the timeline is still on its way, the chrome renders in the wait.
  useEffect(() => {
    if (timelineLoading || commentTasksLoading) releaseLandingChrome();
  }, [timelineLoading, commentTasksLoading, releaseLandingChrome]);
  const handoffRound = useMemo(() => latestSquadHandoffRound(timeline), [timeline]);
  const enteringRunIds = useNewRunIds(id, commentTasks);
  const previousCommentRuns = useRef(new Map<string, CommentRun[]>());
  const { runs: commentRuns, timeline: displayTimeline, standaloneRuns } = useMemo(() => {
    const next = buildCommentRunView(commentTasks ?? [], timeline, previousCommentRuns.current);
    previousCommentRuns.current = next.runs;
    return next;
  }, [commentTasks, timeline]);
  // Scope entering run IDs per thread and reuse unchanged Sets so a new run in
  // one thread never busts React.memo on sibling CommentCards.
  const prevThreadEnteringRef = useRef(new Map<string, ReadonlySet<string>>());
  const enteringRunIdsByThread = useMemo(() => {
    const next = new Map<string, ReadonlySet<string>>();
    if (enteringRunIds.size > 0) {
      const prev = prevThreadEnteringRef.current;
      for (const [rootId, runs] of commentRuns) {
        const matched = runs.filter((run) => enteringRunIds.has(run.task.id)).map((run) => run.task.id);
        if (matched.length === 0) continue;
        const prior = prev.get(rootId);
        if (prior && prior.size === matched.length && matched.every((taskId) => prior.has(taskId))) {
          next.set(rootId, prior);
        } else {
          next.set(rootId, new Set(matched));
        }
      }
    }
    prevThreadEnteringRef.current = next;
    return next;
  }, [commentRuns, enteringRunIds]);

  // Resolve / unresolve must always clear the per-session expand entry so
  // re-resolving an already-expanded thread folds it back to the bar (the
  // expand Set is keyed only on commentId, not on resolution state). Without
  // this wrapper, an expand → unresolve → resolve sequence keeps the thread
  // visually expanded after the second resolve. Read displayTimeline via ref
  // so every timeline mutation does not mint a new callback and bust React.memo
  // on every mounted CommentCard.
  const displayTimelineRef = useRef(displayTimeline);
  displayTimelineRef.current = displayTimeline;
  const handleResolveToggle = useCallback(
    (commentId: string, resolved: boolean) => {
      // Fold the thread back on any resolve change: clear the thread ROOT's
      // expand entry (expand state is keyed on root id, but a resolve target
      // can be a reply). Walk parent_id up to the root.
      const byId = new Map(displayTimelineRef.current.map((e) => [e.id, e]));
      let cur = byId.get(commentId);
      while (cur?.parent_id && byId.get(cur.parent_id)) cur = byId.get(cur.parent_id)!;
      clearResolvedExpand(cur?.id ?? commentId);
      toggleResolveComment(commentId, resolved);
    },
    [clearResolvedExpand, toggleResolveComment],
  );

  // Memoized timeline grouping. Each render rebuilds the per-parent map from
  // the latest timeline, then pre-flattens each thread's reply subtree into a
  // dedicated `threadReplies` slice per root. Slices are stabilized against
  // the previous render via `prevThreadRepliesRef`: if a thread's flat list
  // is shallow-equal to the previous one, we reuse the previous array so
  // React.memo on CommentCard / ResolvedThreadBar can short-circuit. Without
  // this, every WS event (including reactions, edits, AI streaming on an
  // unrelated thread) hands every card a brand-new prop reference and forces
  // every thread subtree to re-render in lockstep.
  const prevThreadRepliesRef = useRef<Map<string, TimelineEntry[]>>(new Map());
  const timelineView = useMemo(() => {
    // Group entries: top-level = activities + root comments; replies are
    // bucketed under their parent's id and rendered nested inside CommentCard.
    // No orphan rescue needed: the timeline is fetched in full, so every
    // reply's parent is always in the same array.
    const topLevel = displayTimeline.filter(
      (e) => e.type === "activity" || !e.parent_id,
    );
    const repliesByParent = new Map<string, TimelineEntry[]>();
    for (const e of displayTimeline) {
      if (e.type === "comment" && e.parent_id) {
        const list = repliesByParent.get(e.parent_id) ?? [];
        list.push(e);
        repliesByParent.set(e.parent_id, list);
      }
    }

    // Pre-flatten each top-level comment's thread subtree (parent + every
    // descendant in render order). Reuse the previous array reference when
    // the thread is unchanged so unrelated CommentCards keep their memo.
    const prevThreadReplies = prevThreadRepliesRef.current;
    const threadReplies = new Map<string, TimelineEntry[]>();
    for (const root of topLevel) {
      if (root.type !== "comment") continue;
      const fresh = collectThreadReplies(root.id, repliesByParent);
      const previous = prevThreadReplies.get(root.id);
      threadReplies.set(
        root.id,
        previous && shallowEqualEntries(previous, fresh) ? previous : fresh,
      );
    }
    prevThreadRepliesRef.current = threadReplies;

    // Coalesce consecutive activities from the same actor + action.
    // - task_completed / task_failed: no time limit (these repeat across runs)
    // - all other actions: within a 2-minute window
    // - squad_leader_evaluated: never coalesce; outcome/reason are audit data
    // The two action lists live in ./timeline-coalesce (pinned to Go by a contract test).
    const COALESCE_MS = 2 * 60 * 1000;
    // Unanchored runs keep their enqueue-time slot while working, then use
    // the published reply's time or the run's end time.
    const entryById = new Map(displayTimeline.map((entry) => [entry.id, entry]));
    const chronological = orderTimelineWithRuns(topLevel, standaloneRuns, entryById);
    const coalesced: (TimelineEntry | CommentRun)[] = [];
    for (const entry of chronological) {
      if ("task" in entry) {
        coalesced.push(entry);
        continue;
      }
      if (entry.type === "activity") {
        const prev = coalesced[coalesced.length - 1];
        if (
          !NEVER_COALESCE_ACTIONS.has(entry.action!) &&
          prev && !("task" in prev) && prev.type === "activity" &&
          prev.action === entry.action &&
          prev.actor_type === entry.actor_type &&
          prev.actor_id === entry.actor_id &&
          (NO_TIME_LIMIT_ACTIONS.has(entry.action!) ||
            Math.abs(new Date(entry.created_at).getTime() - new Date(prev.created_at).getTime()) <= COALESCE_MS)
        ) {
          coalesced[coalesced.length - 1] = { ...entry, coalesced_count: (prev.coalesced_count ?? 1) + 1 };
          continue;
        }
      }
      coalesced.push(entry);
    }

    // Group consecutive activities together so the connector line works
    const groups: RawTimelineGroup[] = [];
    for (const entry of coalesced) {
      if ("task" in entry) {
        groups.push({ type: "run", run: entry, entry: entry.hasReply && entry.commentId ? entryById.get(entry.commentId) : undefined });
      } else if (entry.type === "activity") {
        const last = groups[groups.length - 1];
        if (last?.type === "activities") {
          last.entries.push(entry);
        } else {
          groups.push({ type: "activities", entries: [entry] });
        }
      } else {
        groups.push({ type: "comment", entries: [entry] });
      }
    }

    return { threadReplies, groups };
  }, [displayTimeline, standaloneRuns]);

  // Map of reply-comment id → root-comment id, so a deep-link to a reply
  // (which lives inside a CommentCard, not in the flat items array) can fall
  // back to scrolling the root thread into view. Without this, an inbox
  // notification on a reply would land at items[-1] and short-circuit.
  //
  // Computed here (ahead of `forceThreadOpen` below) because the pin must be
  // derivable from the *requested* target (`highlightCommentId`) alone, never
  // from whether the DOM node was actually found — see that callback's
  // comment for why.
  const replyToRoot = useMemo(() => {
    const map = new Map<string, string>();
    for (const [rootId, replies] of timelineView.threadReplies) {
      for (const reply of replies) {
        map.set(reply.id, rootId);
      }
    }
    return map;
  }, [timelineView.threadReplies]);

  // Readiness predicate handed to `useInPageFind` below (01-DESIGN "no query
  // effect or MutationObserver callback can walk pre-reveal DOM"). Declared
  // as a ref-backed stable callback because `find` (below) is created BEFORE
  // `disclosureReveal`/`revealKey` can be computed — both need `find.open`
  // itself. The callback's identity stays stable across renders; only its
  // ref target is refreshed once `isDisclosureRevealCommitted` exists a few
  // lines down, so `useInPageFind`'s internal `isReadyRef.current = isReady`
  // always reads whatever this render's real readiness check is by the time
  // any effect actually invokes it.
  const findIsReadyRef = useRef<() => boolean>(() => true);
  const findIsReady = useCallback(() => findIsReadyRef.current(), []);
  const revealFindDocumentRef = useRef<(documentId: string) => void>(() => {});
  const revealFindDocument = useCallback((documentId: string) => revealFindDocumentRef.current(documentId), []);
  const findDocuments = useMemo(() => [
    { id: `find-title-${id}`, text: issue?.title ?? "" },
    { id: `find-description-${id}`, text: issue?.description ?? "" },
    ...timelineView.groups.flatMap((group) => {
      const root = group.type === "run" ? group.entry : group.type === "comment" ? group.entries[0] : undefined;
      return root ? [root, ...(timelineView.threadReplies.get(root.id) ?? EMPTY_REPLIES)]
        .filter((entry) => !isDeletedComment(entry))
        .map((entry) => ({ id: `comment-${entry.id}`, text: entry.content ?? "" })) : [];
    }),
  ], [id, issue?.title, issue?.description, timelineView]);

  const find = useInPageFind({
    container: scrollContainerEl,
    contentKey: `${timelineView.groups.length}:${expandedResolved.size}`,
    enabled: !!issue,
    isReady: findIsReady,
    documents: findDocuments,
    revealDocument: revealFindDocument,
  });
  const findCommentId = find.activeDocumentId?.startsWith("comment-") ? find.activeDocumentId.slice(8) : null;
  const findRootId = findCommentId ? replyToRoot.get(findCommentId) ?? findCommentId : null;
  // Close the bar (and drop highlights) when navigating to another issue —
  // the web route reuses this component instead of remounting.
  const closeFind = find.closeFind;
  useEffect(() => {
    closeFind();
  }, [id, closeFind]);

  // Bumped on every false→true find.open transition (a new find "session",
  // 01-DESIGN "find session" token) and reset on issue switch. This — not
  // `find.open` itself — is the identity `useIssueDisclosureReveal` uses to
  // recognize "the same open session" vs. "find closed and reopened": a
  // close-then-reopen with no intervening content change must still get a
  // fresh committed generation rather than reusing a stale one from before
  // the timeline flattened back to virtualized in between.
  const findSessionIdRef = useRef(0);
  const wasFindOpenRef = useRef(false);
  if (find.open && !wasFindOpenRef.current) findSessionIdRef.current += 1;
  wasFindOpenRef.current = find.open;
  useEffect(() => {
    findSessionIdRef.current = 0;
    wasFindOpenRef.current = false;
  }, [id]);

  const effectiveExpandedResolved = useMemo(() => {
    if (!findRootId || expandedResolved.has(findRootId)) return expandedResolved;
    const next = new Set(expandedResolved);
    next.add(findRootId);
    return next;
  }, [findRootId, expandedResolved]);

  // Flat array consumed by <Virtuoso>. Recomputed when timelineView.groups
  // changes (timeline events) or effectiveExpandedResolved flips (user
  // toggles a resolved thread, or find opens/closes). Kept in a useMemo so
  // Virtuoso's data identity is stable across unrelated re-renders.
  const rootItems = useMemo<TimelineItem[]>(
    () => flattenGroups(timelineView.groups, effectiveExpandedResolved),
    [timelineView.groups, effectiveExpandedResolved],
  );

  // The anchor a deep-link target renders under. A failed run's notification
  // names its task, but a run that left a reply (its failure notice included)
  // renders under the reply's id, in a thread slot and at the top level alike.
  const highlightTargetId = useMemo(() => {
    if (!highlightCommentId) return undefined;
    const topLevel = rootItems.find((it) => it.kind === "run" && it.run.task.id === highlightCommentId);
    if (topLevel) return topLevel.id;
    for (const runs of commentRuns.values()) {
      const run = runs.find((candidate) => candidate.task.id === highlightCommentId);
      if (!run) continue;
      const reply = run.hasReply && run.commentId
        ? displayTimeline.find((entry) => entry.id === run.commentId)
        : undefined;
      return reply && !isDeletedComment(reply) ? reply.id : run.task.id;
    }
    return highlightCommentId;
  }, [highlightCommentId, rootItems, commentRuns, displayTimeline]);

  // Thread root of a deep-link target. A run sits in a slot inside the thread
  // it answers, so it unfolds and scrolls with that thread exactly like a
  // reply does.
  const highlightRootId = useMemo(() => {
    if (!highlightTargetId) return undefined;
    const replyRoot = replyToRoot.get(highlightTargetId);
    if (replyRoot) return replyRoot;
    for (const [rootId, runs] of commentRuns) {
      if (runs.some((run) => run.task.id === highlightCommentId)) return rootId;
    }
    return undefined;
  }, [highlightTargetId, highlightCommentId, replyToRoot, commentRuns]);

  // The root whose thread contains the current deep-link/highlight request,
  // derived purely from `highlightCommentId` + `replyToRoot` — NOT from
  // `highlightedId` (only set once the target DOM node is actually found,
  // see the landing effect below) and NOT from a DOM query. A target inside a
  // currently-collapsed/compact thread has no DOM node yet; gating the pin on
  // its own landing result is circular (the thread never opens because the
  // node isn't there, and the node never mounts because the thread never
  // opens). Deriving the pin from the request itself breaks that cycle.
  const targetRootId = useMemo(() => {
    if (!highlightCommentId) return null;
    if (timelineView.threadReplies.has(highlightCommentId)) return highlightCommentId;
    return highlightRootId ?? null;
  }, [highlightCommentId, highlightRootId, timelineView.threadReplies]);

  // Committed reveal-before-DOM-walk generation (01-DESIGN "Find and target
  // reveal lifecycle"). `revealAll` covers BOTH forcing reasons that require
  // waiting for a committed generation before collecting/landing: find.open
  // (every fold opens for search) and an active target/notification pin
  // (targetRootId; forceThreadOpen below already opens its thread, this is
  // what tells the landing effect the opened DOM has actually committed).
  // `revealKey` changes identity whenever either reason starts a NEW request
  // — a fresh find session, or a fresh/replayed highlight request — so a
  // stale commit from the previous request can never be mistaken for the
  // current one. `contentKey` mirrors what actually changes the shape of the
  // revealed DOM: item count and how many resolved roots are currently shown.
  const revealAll = find.open || !!targetRootId;
  const revealKey = find.open
    ? `find:${findSessionIdRef.current}:${find.activeDocumentId ?? "none"}`
    : targetRootId
      ? `target:${highlightCommentId}:${highlightRequestToken ?? 0}`
      : "none";
  const disclosureReveal = useIssueDisclosureReveal({
    issueId: id,
    revealAll,
    revealKey,
    contentKey: `${rootItems.length}:${effectiveExpandedResolved.size}`,
  });
  const isDisclosureRevealCommitted = disclosureReveal.isCommitted;
  // Find's own readiness: while find is closed there is nothing to gate (the
  // hook's default `isReady` behavior — see the `enabled: !!issue` case for
  // the shortcut itself), so only assert the committed-generation check while
  // find is actually open. This is the ref `findIsReady` above reads.
  findIsReadyRef.current = () => !find.open || isDisclosureRevealCommitted(id, revealKey);

  // Root ids whose thread currently carries an in-progress reply draft/
  // upload, OR an active inline edit session on the root or one of its
  // replies. Feeds `forceThreadOpen` below so Show-less-driven hiding can't
  // strand an in-progress interaction out of view (01-DESIGN "New reply /
  // edit / active run": "if active interaction/output would be hidden, latch
  // length-expanded... and pin containing gates").
  //
  // Reply drafts/uploads live under `reply:${issueId}:${rootId}` in
  // useCommentDraftStore. Inline edit sessions are component-local
  // (useEditAttachmentState's `editing` boolean) and NOT directly observable
  // from here — but that hook mirrors its live content into the SAME draft
  // store under `edit:${issueId}:${commentId}` on every edit
  // (comment-card.tsx's ContentEditor onUpdate), and clears that entry the
  // moment the edit session ends (resetState → clearDraft, on both cancel and
  // successful save — see useEditAttachmentState). So "an `edit:` draft
  // exists for this comment id" is a reliable, already-available proxy for
  // "this comment has an active edit session with unsaved content or a
  // pending upload right now" — reachable without lifting `editing` itself to
  // page level.
  //
  // Genuinely NOT covered: focus/selection with no unsaved change yet (e.g.
  // the editor is focused but the user hasn't typed anything, or a caret is
  // placed but no upload started) — that produces no draft-store entry and
  // has no page-level signal today. Lifting bare focus/selection state would
  // need new plumbing (a per-row "focused" callback bubbling to the page);
  // that is out of scope for this fix.
  const draftsByKey = useCommentDraftStore((s) => s.drafts);
  // CHE-1935: `rootIdsWithActiveEditDraft` is the edit-only subset. A bare `reply:` draft is not
  // a reason to force or latch length expansion: the reply box is already visible in the compact
  // state, so unfolding the hidden replies only pushes it away from the user. Only an edit can
  // need a hidden reply mounted.
  const rootIdsWithActiveEditDraft = useMemo(() => {
    const ids = new Set<string>();
    const hasDraftContentOrUpload = (key: string) => {
      const draft = draftsByKey[key];
      if (!draft) return false;
      return draft.content.trim().length > 0 || draft.attachments.some((u) => u.status === "uploading");
    };
    for (const [rootId, replies] of timelineView.threadReplies) {
      if (
        hasDraftContentOrUpload(`edit:${id}:${rootId}`) ||
        replies.some((reply) => hasDraftContentOrUpload(`edit:${id}:${reply.id}`))
      ) {
        ids.add(rootId);
      }
    }
    return ids;
  }, [draftsByKey, id, timelineView.threadReplies]);

  // CHE-5437: what may pin a thread open against an explicit manual Collapse.
  // Reply TEXT does not: it lives in the draft store, so the composer restores
  // it on expand. An upload in flight or an inline edit does: both are
  // component-local and would be lost on unmount.
  const rootIdsPinnedByLocalWork = useMemo(() => {
    const ids = new Set<string>();
    const uploading = (key: string) => !!draftsByKey[key]?.attachments.some((u) => u.status === "uploading");
    const editing = (key: string) => uploading(key) || (draftsByKey[key]?.content.trim().length ?? 0) > 0;
    for (const [rootId, replies] of timelineView.threadReplies) {
      if (uploading(`reply:${id}:${rootId}`) || editing(`edit:${id}:${rootId}`)
        || replies.some((reply) => editing(`edit:${id}:${reply.id}`))) ids.add(rootId);
    }
    return ids;
  }, [draftsByKey, id, timelineView.threadReplies]);

  // Root ids with an active (queued/running) run that has no published reply
  // yet, anchored to the root itself or to one of the root's replies. Split
  // out from `forceThreadOpen` below so it can also feed the latch effect
  // after it — the two need the exact same "active run" definition, and
  // `rootIdsWithActiveEditDraft` already established the pattern of
  // precomputing a root-id set for this kind of reason.
  //
  // `commentRuns` is keyed by THREAD ROOT id, not by reply id (see
  // comment-runs.ts's `buildCommentRunView`: every run's group walks
  // `anchorCommentId` up through `parent_id` to the thread root before being
  // stored) — the same key `runs={commentRuns.get(item.id)}` uses below when
  // rendering a root's CommentCard. A run anchored to one of a root's
  // replies is filed under the ROOT's id, so this reads `commentRuns.get(rootId)`
  // once per root and checks every run's `anchorCommentId` against that
  // root's replies, rather than incorrectly looking up `commentRuns` by each
  // reply's own id (which only holds runs for a reply that is itself a
  // thread root, e.g. a standalone assignment answer).
  //
  // `anchorCommentId === rootId` is included alongside the replies check: a
  // root-anchored run renders inside CommentCard's own `open` gate
  // (comment-card.tsx's root-anchored AgentRunComment path), so without this
  // a manually collapsed root could hide a newly active run anchored to the
  // root itself instead of one of its replies.
  const rootIdsWithActiveRun = useMemo(() => {
    const ids = new Set<string>();
    for (const [rootId, replies] of timelineView.threadReplies) {
      const rootRuns = commentRuns.get(rootId) ?? EMPTY_COMMENT_RUNS;
      const replyIds = new Set(replies.map((reply) => reply.id));
      if (rootRuns.some((run) => !run.hasReply && isActiveCommentRun(run.task)
        && run.anchorCommentId
        && (run.anchorCommentId === rootId || replyIds.has(run.anchorCommentId)))) {
        ids.add(rootId);
      }
    }
    return ids;
  }, [timelineView.threadReplies, commentRuns]);

  // Root ids whose DOM subtree (the `id="comment-${rootId}"` wrapper
  // `renderItem` renders below, which contains the root AND every nested
  // reply — see comment-card.tsx's `id="comment-${slot.comment.id}"` for
  // replies) currently contains either the focused element or a
  // non-collapsed text selection, with NO unsaved draft/edit present. This
  // is CHE-476's restoration of 01-DESIGN line 51's original scope, which
  // `rootIdsWithActiveEditDraft` above deliberately does not cover (see its
  // "Genuinely NOT covered" note) — a focused composer with no typed content
  // yet, or a caret/selection with no edit in progress, produces no
  // draft-store entry.
  //
  // Recomputed from `focusin`/`focusout`/`selectionchange` rather than React
  // focus-capture props: unlike `descriptionFocused` above (one fixed
  // element), this needs to test an arbitrary focus/selection target against
  // an arbitrary number of root subtrees, so a single document-level
  // listener plus `Element.contains` is the direct check — no per-row prop
  // plumbing needed.
  const [rootIdsWithActiveFocusOrSelection, setRootIdsWithActiveFocusOrSelection] = useState<Set<string>>(EMPTY_ID_SET);
  const threadReplies = timelineView.threadReplies;
  useEffect(() => {
    const recompute = () => {
      const active = document.activeElement;
      const sel = document.getSelection();
      // A collapsed (caret-only) selection is not a "selection" for this
      // purpose — AC 4 requires a real range before it can block Show less.
      // Anchor (not the range's commonAncestorContainer) is the containment
      // test: 01-DESIGN-v3.md:51 blocks collapse when the SELECTION's anchor
      // sits in the collapsing subtree, not when the range's nearest common
      // DOM ancestor does. A cross-boundary selection that starts in-thread
      // and ends outside it has a commonAncestorContainer outside the
      // thread, which would wrongly let Show less hide the anchored text.
      const selAnchor = sel && !sel.isCollapsed && sel.rangeCount > 0 ? sel.anchorNode : null;
      const ids = new Set<string>();
      for (const node of [active, selAnchor]) {
        const element = node instanceof Element ? node : node?.parentElement;
        // CHE-5437: pressing Collapse focuses that button inside the thread.
        // Its own focus is not an interaction to protect from hiding.
        if (element?.closest("[data-thread-toggle]")) continue;
        const rootId = element?.closest<HTMLElement>("[data-thread-root]")?.dataset.threadRoot;
        if (rootId) ids.add(rootId);
      }
      setRootIdsWithActiveFocusOrSelection((prev) => {
        if (prev.size === ids.size && [...prev].every((id) => ids.has(id))) return prev;
        return ids;
      });
    };
    recompute();
    document.addEventListener("focusin", recompute);
    document.addEventListener("focusout", recompute);
    document.addEventListener("selectionchange", recompute);
    return () => {
      document.removeEventListener("focusin", recompute);
      document.removeEventListener("focusout", recompute);
      document.removeEventListener("selectionchange", recompute);
    };
  }, [threadReplies]);

  // Whether `rootId`'s thread must be forced fully open regardless of the
  // durable length preference AND regardless of manual collapse (01-DESIGN
  // "Effective order" priority 1 — find/target pin outranks priority 3,
  // manual collapse). Reasons that can force a thread open:
  //   - `targetRootId` — a deep-link/highlight request targets this thread,
  //     computed from the request itself (see above), so it fires the moment
  //     the request is known, before any DOM lookup.
  //   - `rootIdsWithActiveRun` — an active (queued/running) run with no
  //     reply yet, anchored to one of this thread's replies.
  //   - an in-progress reply draft/upload, or an active inline edit session
  //     on the root or a reply, anywhere in this thread
  //     (`rootIdsWithActiveEditDraft` above — see its comment for exactly
  //     what is and is not covered).
  // This does not persist a length or collapse choice by itself — it only
  // lifts the ceiling for the duration of the reason. Two transition-matrix
  // rows additionally require LATCHING the length preference on a
  // not-yet-expanded root so the expansion survives after the reason ends
  // (see the effect below): "New reply / edit / active run" (active
  // reply/edit, active run) and "Target reveal / notification replay"
  // (`targetRootId`, latch its length-expanded choice). "Find open / close"
  // is the one reason here that does NOT latch — row 58 is explicit that
  // find writes no fold preference.
  // CHE-5437: roots the user collapsed by pressing the button. A press outranks
  // the soft pins (target, highlight, focus/selection); find, an active run and
  // unsaved local work still hold the thread open.
  const [manualCollapsedRoots, setManualCollapsedRoots] = useState<ReadonlySet<string>>(EMPTY_ID_SET);
  const handleManualCollapseChange = useCallback((rootId: string, collapsed: boolean) => {
    setManualCollapsedRoots((prev) => {
      if (prev.has(rootId) === collapsed) return prev;
      const next = new Set(prev);
      if (collapsed) next.add(rootId); else next.delete(rootId);
      return next;
    });
  }, []);
  // A new deep-link request re-targets a thread: the earlier press no longer applies.
  useEffect(() => {
    setManualCollapsedRoots(EMPTY_ID_SET);
  }, [highlightCommentId, highlightRequestToken]);

  const forceThreadOpen = useCallback((rootId: string): boolean => {
    if (findRootId === rootId) return true;
    if (rootIdsPinnedByLocalWork.has(rootId)) return true;
    if (rootIdsWithActiveRun.has(rootId)) return true;
    if (manualCollapsedRoots.has(rootId)) return false;
    if (targetRootId === rootId) return true;
    if (rootIdsWithActiveFocusOrSelection.has(rootId)) return true;
    const threadRepliesForRoot = timelineView.threadReplies.get(rootId) ?? EMPTY_REPLIES;
    if (highlightedId && (highlightedId === rootId || threadRepliesForRoot.some((r) => r.id === highlightedId))) {
      return true;
    }
    return false;
  }, [findRootId, targetRootId, rootIdsPinnedByLocalWork, rootIdsWithActiveRun, rootIdsWithActiveFocusOrSelection, manualCollapsedRoots, timelineView.threadReplies, highlightedId]);

  // CHE-554: same reasons as `forceThreadOpen` above, MINUS a bare
  // focus/selection-only pin. `forceThreadOpen` correctly forces `open` (the
  // whole replies section mounts) for a focus/selection pin — that's what
  // lets a deep-linked/focused reply's own DOM node exist at all. But
  // CommentCard's Show-more button unmounts the instant `lengthExpanded`
  // flips true (its render guard is `!lengthExpanded && hiddenCount > 0`),
  // and the button's own `mousedown` is exactly what puts it inside
  // `rootIdsWithActiveFocusOrSelection` in the first place (mousedown's
  // default action focuses the pressed button, firing `focusin` before
  // `click` completes) — so gating the button's mount on `forceThreadOpen`
  // directly would make the control unmount out from under its own press,
  // and the `click` that should call `onThreadLengthExpandChange` would land
  // on a detached node. Passed to CommentCard as `forceThreadLengthExpanded`
  // (see its doc comment); `forceThreadExpanded` is still passed unchanged
  // for `open` and for gating Show less. Every other force-open reason here
  // (find, target/deep-link, active reply draft, active run, highlighted
  // reply) still forces the length open exactly as before — only a pin
  // caused SOLELY by focus/selection is excluded.
  const forceThreadLengthExpand = useCallback((rootId: string): boolean => {
    if (findRootId === rootId) return true;
    if (targetRootId === rootId) return true;
    if (rootIdsWithActiveEditDraft.has(rootId)) return true;
    if (rootIdsWithActiveRun.has(rootId)) return true;
    const threadRepliesForRoot = timelineView.threadReplies.get(rootId) ?? EMPTY_REPLIES;
    if (highlightedId && (highlightedId === rootId || threadRepliesForRoot.some((r) => r.id === highlightedId))) {
      return true;
    }
    return false;
  }, [findRootId, targetRootId, rootIdsWithActiveEditDraft, rootIdsWithActiveRun, timelineView.threadReplies, highlightedId]);

  const collapsedThreadIds = useCommentCollapseStore(state => state.collapsedByIssue[id]);
  const items = useMemo<TimelineItem[]>(() => rootItems.flatMap((item): TimelineItem[] => {
    if (item.kind !== "comment" && item.kind !== "run") return [item];
    const entry = item.entry;
    if (!entry || (entry.resolved_at && !effectiveExpandedResolved.has(entry.id))) return [item];
    const { rows } = projectCommentThread(entry, timelineView.threadReplies.get(entry.id) ?? EMPTY_REPLIES,
      commentRuns.get(entry.id) ?? EMPTY_COMMENT_RUNS,
      expandedThreadLengths.has(entry.id) || forceThreadLengthExpand(entry.id),
      effectiveExpandedResolved.has(entry.id),
      !collapsedThreadIds?.includes(entry.id) || forceThreadOpen(entry.id));
    return [{ ...item, row: rows[0] }, ...rows.slice(1).map((row): TimelineItem => ({
      kind: "thread-row", id: row.id, entry, row,
    }))];
  }), [rootItems, effectiveExpandedResolved, timelineView.threadReplies, commentRuns,
    expandedThreadLengths, forceThreadLengthExpand, collapsedThreadIds, forceThreadOpen]);

  // Latch: 01-DESIGN line 56 ("New reply / edit / active run") and line 57
  // ("Target reveal / notification replay") both require that when their
  // reason forces a thread open, the length-expanded preference is PERSISTED
  // for that root — "ending the reason releases its temporary pin but does
  // not clear the latched length expansion." Line 58 ("Find open / close")
  // is explicit that find does the opposite — "no write to any fold
  // preference" — so `find.open` is deliberately absent from this effect;
  // it still forces threads open via `forceThreadOpen` above, it just never
  // latches. Without this effect, `forceThreadOpen`'s draft/run/target pins
  // are temporary only: the moment the draft is sent, the run finishes, or
  // the target reveal releases, the pin drops and a previously-compact root
  // refolds, silently discarding content the user was just looking at
  // because of that reason.
  //
  // Effect (not a write inside `forceThreadOpen` itself) so this fires once
  // per root per reason-start rather than on every render while the reason
  // is still active — `setThreadExpanded` is idempotent (issue-disclosure-
  // store.ts already no-ops when the value is unchanged), but an effect
  // keyed on the reason-set's membership is the correct place for a reason
  // "starting" rather than "continuing", and avoids calling a store setter
  // during render.
  //
  // CHE-479 fix — Fold All vs. the latch: this effect's membership check
  // (`!expandedThreadLengths.has(rootId)`) cannot by itself tell "never
  // expanded" apart from "Fold All (search-command.tsx's
  // foldAllCommentThreads -> collapseAllThreads) just cleared every root's
  // membership a moment ago." Fold All clears `expandedThreadIdsByIssue`
  // wholesale, which changes this effect's `expandedThreadLengths`
  // dependency and re-fires it — so if a draft/run/target reason was already
  // active before Fold All ran, a membership-only check would immediately
  // re-latch that root open, undermining the fold.
  //
  // `justFoldedRoots` (from `useIssueDisclosureStore`'s
  // `justFoldedRootIdsByIssue`, written by `collapseAllThreads`) records
  // exactly which roots the most recent Fold All cleared, as a fresh Set
  // instance per Fold All call. Its identity is "which fold occurrence";
  // `suppressedFoldRootsRef` (seeded from it) tracks which of those roots
  // are still suppressed for THIS occurrence. A suppressed root is released
  // the moment EITHER:
  //   (a) a later Fold All produces a genuinely new `justFoldedRoots`
  //       instance — `lastSeenJustFoldedRootsRef` detects this and resets
  //       both `suppressedFoldRootsRef` and `voidedFoldRootsRef` from
  //       scratch for the new occurrence, or
  //   (b) this exact root's own latch reason genuinely ENDS (drops out of
  //       `rootIdsWithActiveEditDraft` / `rootIdsWithActiveRun`, or stops
  //       being `targetRootId`) for at least one render — recorded in
  //       `voidedFoldRootsRef` so the release persists even though
  //       `justFoldedRoots` itself hasn't changed identity, e.g. the user
  //       clears a reply draft and starts a brand new one on the same root
  //       with no further Fold All in between. 01-DESIGN calls this a fresh
  //       "reason-start", which must always take priority over a stale fold
  //       suppression.
  // Neither condition depends on `expandedThreadLengths` catching up to the
  // fold, and neither needs a second store write just to expire itself.
  //
  // Known tension, not resolved here: the store only records current
  // membership, not *why* a root is at its current expansion state, beyond
  // the one specific "just fold-all'd" case above. If a user explicitly
  // clicks Show less on a root (no Fold All involved) and then, in the same
  // session, starts a reply draft on that same still-compact root, this
  // effect cannot distinguish "never expanded" from "the user just chose
  // compact" — both read as `!expandedThreadLengths.has(rootId)` — and will
  // latch it open again. 01-DESIGN line 56 reads as unconditional ("Idle
  // updates cannot refold a user-expanded thread" is about idle updates, not
  // about this case), so this implements the literal requirement; closing
  // that remaining gap for real would need the store to also record *why* a
  // root is compact in general (e.g. an explicit-collapse marker distinct
  // from "default"), which is new store surface beyond what this fix was
  // scoped to add.
  useEffect(() => {
    // Reset both refs the moment `justFoldedRoots` becomes a new instance —
    // a distinct Fold All occurrence starts with a clean slate, regardless
    // of what the previous occurrence had suppressed or voided.
    if (lastSeenJustFoldedRootsRef.current !== justFoldedRoots) {
      lastSeenJustFoldedRootsRef.current = justFoldedRoots;
      suppressedFoldRootsRef.current = new Set(justFoldedRoots);
      voidedFoldRootsRef.current = new Set();
    }
    const suppressed = suppressedFoldRootsRef.current;
    const voided = voidedFoldRootsRef.current;

    // A suppressed root whose OWN reason is not active right now has had
    // that reason end — release it from suppression for good (until the
    // NEXT distinct Fold All resets the refs above), so a later fresh
    // reason-start on the same root latches normally even though
    // `justFoldedRoots` itself hasn't changed identity.
    for (const rootId of suppressed) {
      const isActive =
        rootIdsWithActiveEditDraft.has(rootId) || rootIdsWithActiveRun.has(rootId) || rootId === targetRootId;
      if (!isActive) voided.add(rootId);
    }
    for (const rootId of voided) suppressed.delete(rootId);

    for (const rootId of rootIdsWithActiveEditDraft) {
      if (suppressed.has(rootId)) continue;
      if (!expandedThreadLengths.has(rootId)) setThreadExpanded(id, rootId, true);
    }
    for (const rootId of rootIdsWithActiveRun) {
      if (suppressed.has(rootId)) continue;
      if (!expandedThreadLengths.has(rootId)) setThreadExpanded(id, rootId, true);
    }
    if (targetRootId && !suppressed.has(targetRootId) && !expandedThreadLengths.has(targetRootId)) {
      setThreadExpanded(id, targetRootId, true);
    }
  }, [
    rootIdsWithActiveEditDraft,
    rootIdsWithActiveRun,
    targetRootId,
    expandedThreadLengths,
    justFoldedRoots,
    id,
    setThreadExpanded,
  ]);

  // ID of the trailing activity block — the only one expanded by default.
  const lastActivityGroupId = useMemo(() => {
    for (let i = timelineView.groups.length - 1; i >= 0; i--) {
      const g = timelineView.groups[i]!;
      if (g.type === "activities") return g.entries[0]!.id;
    }
    return null;
  }, [timelineView.groups]);
  // Squad handoffs say who holds the issue between comments, so their blocks
  // open by default like the trailing one.
  const handoffActivityGroupIds = useMemo(() => {
    const ids = new Set<string>();
    for (const g of timelineView.groups) {
      if (g.type === "activities" && g.entries.some((e) => e.action === "squad_handoff")) ids.add(g.entries[0]!.id);
    }
    return ids;
  }, [timelineView.groups]);

  // Deep-link target index in the flat items array. For root comments this is
  // a direct findIndex hit; for replies and threaded runs we look up the
  // enclosing root.
  const targetIdx = useMemo(() => {
    if (!highlightTargetId) return -1;
    const threadRootId = highlightRootId ?? highlightTargetId;
    const landingId = commentLandingTarget(highlightTargetId, threadRootId, timelineView.threadReplies.get(threadRootId) ?? EMPTY_REPLIES);
    const direct = items.findIndex((it) => it.id === landingId);
    if (direct >= 0) return direct;
    if (!highlightRootId) return -1;
    return items.findIndex((it) => it.id === highlightRootId);
  }, [items, highlightTargetId, highlightRootId, timelineView.threadReplies]);

  // One entry per comment thread (folded resolved bars included), activity
  // groups skipped. Derived from the same flat `items` array Virtuoso renders
  // so the right-edge outline always matches the page order.
  //
  // The resolved flag comes from `deriveThreadResolution`, not from the
  // `resolved-bar` kind: that kind only covers root resolutions that are
  // currently folded, so it would miss reply resolutions and would flip off
  // as soon as the user expanded a resolved thread.
  const minimapThreads = useMemo<ThreadMinimapThread[]>(
    () =>
      items.flatMap((it) => {
        if (it.kind === "activity-group" || it.kind === "thread-row" || !it.entry) return [];
        const replies = timelineView.threadReplies.get(it.id) ?? EMPTY_REPLIES;
        const resolution = deriveThreadResolution(it.entry, replies);
        return [
          {
            id: it.id,
            entry: it.entry,
            resolved: resolution.kind !== "none",
            participants: collectThreadParticipants(it.entry, replies),
            // Tombstones render no row, so they get no tick either.
            replies: replies.filter((reply) => !isDeletedComment(reply)),
            resolutionReplyId: resolution.kind === "reply" ? resolution.resolutionId : null,
          },
        ];
      }),
    [items, timelineView.threadReplies],
  );

  const virtuosoRef = useRef<VirtuosoHandle>(null);
  // Whether Virtuoso's rendered rows (overscan included) stop short of the
  // last one; rows past them are only estimated. Settled from Virtuoso's own
  // range report, not `items.length`: a comment appended at the bottom is
  // unrendered for a frame, and that frame must not read as "away".
  const timelineItemCountRef = useRef(items.length);
  timelineItemCountRef.current = items.length;
  const [timelineTail, setTimelineTail] = useState({ issueId: id, pending: false });
  const handleTimelineRangeChanged = useCallback((range: ListRange) => {
    const pending = range.endIndex < timelineItemCountRef.current - 1;
    setTimelineTail((prev) => (prev.issueId === id && prev.pending === pending ? prev : { issueId: id, pending }));
  }, [id]);
  // Which rows Virtuoso has mounted, for the thread rail. A store rather than
  // state: a scroll must not re-render this page. A new issue remounts
  // Virtuoso, which reports its range afresh.
  const [rowRange] = useState(createRowRangeSource);
  const onRowRangeChanged = useCallback(
    (range: ListRange) => {
      rowRange.set({ start: range.startIndex, end: range.endIndex });
      handleTimelineRangeChanged(range);
    },
    [rowRange, handleTimelineRangeChanged],
  );
  const mountedRows = useMemo<MountedRows>(
    () => ({ rowIds: items.map((item) => item.id), range: rowRange }),
    [items, rowRange],
  );
  // Per timeline row, whether it mentions the viewer: the root row its own
  // content, each reply row that reply's. Controls, footers and runs never do.
  const mentionsViewerText = useMentionsViewer();
  const selfMentionFlags = useMemo(
    () =>
      items.map((item) => {
        if (item.kind === "comment" || item.kind === "resolved-bar") return mentionsViewerText(item.entry.content);
        if (item.kind !== "thread-row" || item.row.kind !== "reply") return false;
        const reply = timelineView.threadReplies.get(item.entry.id)?.find((r) => r.id === item.row.id);
        return mentionsViewerText(reply?.content);
      }),
    [items, timelineView.threadReplies, mentionsViewerText],
  );
  const jumpToSelfMention = useCallback(
    (index: number) => virtuosoRef.current?.scrollToIndex({ index, align: "start", offset: -16 }),
    [],
  );
  revealFindDocumentRef.current = (documentId) => {
    if (!documentId.startsWith("comment-")) {
      const element = document.getElementById(documentId);
      if (element && scrollContainerEl) {
        scrollContainerEl.scrollTop += element.getBoundingClientRect().top - scrollContainerEl.getBoundingClientRect().top - 80;
      }
      return;
    }
    const commentId = documentId.slice(8);
    const rootId = replyToRoot.get(commentId) ?? commentId;
    const directIndex = items.findIndex((item) => item.id === commentId);
    const index = directIndex >= 0 ? directIndex : items.findIndex((item) => item.id === rootId);
    if (index >= 0) virtuosoRef.current?.scrollToIndex({ index, align: "start", offset: -16 });
  };

  // 01-DESIGN "On close, clear highlights and pending collectors first.
  // Capture nearest comment/root ID and screen offset around the current
  // match (or current viewport) and restore a valid visible anchor after
  // folds and Virtuoso return... preserve offset where possible... No-match
  // open/close uses the entry viewport anchor."
  //
  // This must be the PRE-open viewport, not a value read after find.open has
  // already flipped true: `find.open` becoming true and `items`/
  // `effectiveExpandedResolved` re-flattening to the fully-revealed shape
  // happen in the same render (see effectiveExpandedResolved above), so an
  // effect keyed on `[find.open]` only ever sees the DOM AFTER every fold
  // already force-opened — it can no longer tell which row the user was
  // actually looking at before the reveal. Capturing continuously on every
  // render WHILE FIND IS CLOSED instead means the ref already holds the last
  // pre-open snapshot the instant `find.open` flips, without racing the
  // commit that reveals everything. (Once find is open, `findActiveAnchorRef`
  // below supersedes this as "the current match" — this ref is frozen the
  // moment find opens and only read back on close as the no-match fallback.)
  const findEntryAnchorRef = useRef<{ rootId: string; offset: number } | null>(null);
  // Tracks whether find was open on the PREVIOUS commit — distinct from
  // `wasFindOpenForCloseRef` below (that one gates the restore effect and
  // updates on every close-transition render; this one must specifically
  // suppress capture on the true→false transition render itself, since by
  // then the DOM this layout effect would read already reflects whatever
  // closing find just changed, e.g. a scroll the user made or a fold that
  // resettled — capturing here would silently overwrite the correct
  // already-frozen pre-open snapshot with that post-close state instead of
  // ever handing it to the restore effect that reads it next.
  const wasFindOpenForCaptureRef = useRef(false);
  useLayoutEffect(() => {
    const wasOpenLastCommit = wasFindOpenForCaptureRef.current;
    wasFindOpenForCaptureRef.current = find.open;
    // Freeze the snapshot the instant find opens, and again exactly once on
    // the transition back to closed (the transition render's own DOM read
    // would race the restore that immediately follows) — recapture only in
    // the closed steady state that precedes an OPEN.
    if (find.open || wasOpenLastCommit) return;
    const container = scrollContainerEl;
    if (!container) return;
    const containerRect = container.getBoundingClientRect();
    let nearestId: string | null = null;
    let nearestOffset = 0;
    let nearestDelta = Number.POSITIVE_INFINITY;
    for (const item of items) {
      if (item.kind === "activity-group") continue;
      const el = document.getElementById(`comment-${item.id}`);
      if (!el) continue;
      const top = el.getBoundingClientRect().top - containerRect.top;
      const delta = Math.abs(top);
      if (delta < nearestDelta) {
        nearestDelta = delta;
        nearestId = item.id;
        nearestOffset = top;
      }
    }
    if (nearestId) findEntryAnchorRef.current = { rootId: nearestId, offset: nearestOffset };
  });

  // The active match's containing root/bar id, updated every time the active
  // match changes — this supersedes the entry anchor as "the current match"
  // once the user has a query with results, per 01-DESIGN "around the
  // current match (or current viewport)".
  const findActiveAnchorRef = useRef<string | null>(null);
  useEffect(() => {
    if (find.open) findActiveAnchorRef.current = findCommentId;
  }, [find.open, findCommentId]);

  // 01-DESIGN "On close... restore a valid visible anchor after folds and
  // Virtuoso return: same comment if visible, otherwise its root/bar;
  // preserve offset where possible, clamp within the issue scroll container.
  // If focus is in content that would hide, keep an interaction pin until
  // blur; otherwise move focus from the closing bar to a stable issue
  // control. No stale snapshot writes: restore by removing the overlay,
  // honoring any fold/unfold changes made while find was open."
  //
  // Runs on the true→false find.open transition. The anchor id resolves to
  // its ROOT for the lookup (`replyToRoot`) because closing find drops
  // `forceThreadOpen`'s pin — a reply-level anchor may no longer have its own
  // DOM node once manual/length folds re-apply, but its root/bar always does.
  const wasFindOpenForCloseRef = useRef(false);
  useEffect(() => {
    const wasOpen = wasFindOpenForCloseRef.current;
    wasFindOpenForCloseRef.current = find.open;
    if (!wasOpen || find.open) return;

    // No-match (or never-searched) close falls back to the pre-open entry
    // anchor and its captured offset — 01-DESIGN: "preserve offset where
    // possible". A live active match instead re-centers on itself: the user
    // was just LOOKING at that match, so centering it is the correct
    // restoration, not a regression from the entry anchor's offset (which
    // describes a different row's original position, not the match's).
    const activeAnchorId = findActiveAnchorRef.current;
    const entryAnchor = findEntryAnchorRef.current;
    const anchorId = activeAnchorId ?? entryAnchor?.rootId ?? null;
    const entryOffset = activeAnchorId ? null : (entryAnchor?.offset ?? null);
    findActiveAnchorRef.current = null;
    findEntryAnchorRef.current = null;
    if (!anchorId) return;

    const rootId = replyToRoot.get(anchorId) ?? anchorId;

    // An active interaction (focus/selection with no unsaved change — the
    // same signal `forceThreadOpen` above reads) inside the anchor's thread
    // keeps its own pin alive independent of find, so the content this
    // effect would otherwise need to "restore into view" is already staying
    // visible via that pin. Moving focus away here would fight that: leave
    // it alone and let the interaction's own blur handling (already wired
    // where that pin is read) govern when the pin releases.
    if (rootIdsWithActiveFocusOrSelection.has(rootId)) return;

    // Defer one frame: closing find flips several fold-affecting values in
    // the same tick (forceThreadOpen's pin drops, effectiveExpandedResolved
    // recomputes) — the restore
    // target's DOM needs that commit to land first, exactly like every other
    // scroll-driving effect in this component (see the deep-link landing
    // effect's own rAF centering).
    const raf = requestAnimationFrame(() => {
      const container = scrollContainerEl;
      if (!container) return;
      // Prefer the exact anchor comment if it's still directly visible
      // (thread stayed expanded via a latch or manual preference); otherwise
      // fall back to its root/bar, which is always present once folds
      // resettle.
      const restoredExactRow = !!document.getElementById(`comment-${anchorId}`);
      const el =
        document.getElementById(`comment-${anchorId}`) ??
        document.getElementById(`comment-${rootId}`);
      if (!el) return;
      const containerRect = container.getBoundingClientRect();
      const elRect = el.getBoundingClientRect();
      if (entryOffset !== null && restoredExactRow) {
        // No-match/no-query close on the exact pre-open row: put it back at
        // the offset it held before find opened, clamped to the container,
        // rather than re-centering it — re-centering would move content the
        // user never asked to move just because find happened to open.
        const currentOffset = elRect.top - containerRect.top;
        const target = Math.max(0, container.scrollTop + (currentOffset - entryOffset));
        container.scrollTop = target;
      } else {
        // A live active match, or the exact entry row no longer exists
        // (folds resettled to a different shape) — center on whatever
        // anchor/root DOM node is actually available now.
        const pad = 80;
        const above = elRect.top < containerRect.top + pad;
        const below = elRect.bottom > containerRect.bottom - pad;
        if (above || below) {
          const offsetWithin = elRect.top - containerRect.top + container.scrollTop;
          const target = offsetWithin - container.clientHeight / 2 + elRect.height / 2;
          container.scrollTop = Math.max(0, target);
        }
      }
      // Move focus from the (now unmounted) find bar to a stable issue
      // control — never leave focus stranded on a removed element.
      rightSidebarShortcutTargetRef.current?.focus({ preventScroll: true });
    });
    return () => cancelAnimationFrame(raf);
  }, [find.open, replyToRoot, scrollContainerEl, rootIdsWithActiveFocusOrSelection]);

  // Mobile shows it only near the top of the page, where it covers the text
  // column the least; desktop shows it whenever the latest entry is more than
  // a screen away.
  const jumpToLatest = useJumpToLatest(scrollContainerEl, {
    nearTopOnly: isMobile,
    tailPending: timelineTail.issueId === id && timelineTail.pending,
  });
  // Keyed to `away`, not `visible`: mobile hides the button past the top of
  // the page, but the reader has not reached the new comments there.
  const newComments = useNewCommentsWhileAway(timeline, {
    ready: !timelineLoading,
    away: jumpToLatest.away,
    userId: user?.id,
    resetKey: id,
  });
  // Scroll a freshly posted comment into view, aligned so its bottom sits just
  // above the sticky composer (never behind it). A reply lives inside its root
  // CommentCard, so the containing top-level row is the scroll target.
  // Virtuoso owns the scroll. The first jump materializes the row at its
  // estimated height; once measured, the repeat aligns using the real height.
  // The extra `offset` lifts the row's bottom clear of the sticky composer.
  // `scrollContainerEl` stays out of the deps: a container re-measure would
  // cancel the scroll mid-flight.
  const scrollContainerElRef = useRef<HTMLDivElement | null>(null);
  scrollContainerElRef.current = scrollContainerEl;
  useEffect(() => {
    if (!pendingPostedCommentId) return;
    const rootId = replyToRoot.get(pendingPostedCommentId) ?? pendingPostedCommentId;
    const directIndex = items.findIndex((item) => item.id === pendingPostedCommentId);
    const index = directIndex >= 0 ? directIndex : items.findIndex((item) => item.id === rootId);
    if (index < 0) return;
    let timer: number | undefined;
    let cancelled = false;
    const scrollEnd = () => {
      if (cancelled) return;
      const composer = composerRef.current;
      // The Inbox's group floats above the composer and covers the row too.
      const group = composer?.parentElement?.querySelector("[data-inbox-bar]");
      const offset =
        (composer?.getBoundingClientRect().height ?? 0) +
        (group ? group.getBoundingClientRect().height + COMPOSER_DOCK_GAP : 0);
      virtuosoRef.current?.scrollToIndex({ index, align: "end", offset });
    };
    const container = scrollContainerElRef.current;
    const stopOnUserInput = () => {
      cancelled = true;
      cancelAnimationFrame(frame);
      if (timer !== undefined) window.clearTimeout(timer);
      setPendingPostedCommentId(null);
    };
    const userEvents = ["wheel", "touchstart", "pointerdown"] as const;
    for (const type of userEvents) container?.addEventListener(type, stopOnUserInput, { passive: true });
    const frame = requestAnimationFrame(() => {
      scrollEnd();
      timer = window.setTimeout(() => {
        scrollEnd();
        setPendingPostedCommentId(null);
      }, 100);
    });
    return () => {
      cancelled = true;
      cancelAnimationFrame(frame);
      if (timer !== undefined) window.clearTimeout(timer);
      for (const type of userEvents) container?.removeEventListener(type, stopOnUserInput);
    };
  }, [pendingPostedCommentId, items, replyToRoot]);
  const jumpFlashTimerRef = useRef<number | null>(null);
  useEffect(
    () => () => {
      if (jumpFlashTimerRef.current !== null) window.clearTimeout(jumpFlashTimerRef.current);
    },
    [],
  );
  // Flash the landed comment the same way inbox deep-links do, so the eye has
  // an anchor after the instant jump. (Folded resolved bars don't take the
  // highlight prop — the scroll itself is the feedback there.)
  const flashJumpTarget = useCallback((commentId: string, duration = 2000) => {
    setHighlightedId(commentId);
    if (jumpFlashTimerRef.current !== null) window.clearTimeout(jumpFlashTimerRef.current);
    jumpFlashTimerRef.current = window.setTimeout(() => setHighlightedId(null), duration);
  }, []);
  // Jump to a mounted comment, such as a reply inside a rendered thread. Drive the container's scrollTop directly — never native
  // scrollIntoView, which also scrolls the desktop shell (#3929).
  const jumpToThread = useCallback(
    (threadId: string) => {
      // The target row may not be mounted, so scroll by index and let
      // Virtuoso mount it. Offset leaves a small top gap.
      const index = items.findIndex((it) => it.id === threadId);
      if (index < 0) return;
      virtuosoRef.current?.scrollToIndex({ index, align: "start", offset: -16 });
      flashJumpTarget(threadId);
    },
    [items, flashJumpTarget],
  );
  // Minimap jump to a reply. A reply's anchor exists only while its thread is
  // open, so first undo whatever hides it — the reader's own collapse, a root
  // resolution folding the whole thread into a bar, or a reply resolution
  // folding the other replies — then mount the thread and let the effect
  // below align the reply once its row lands.
  const [pendingReplyJump, setPendingReplyJump] = useState<{ replyId: string; rootId: string } | null>(null);
  const jumpToReply = useCallback(
    (replyId: string) => {
      const rootId = replyToRoot.get(replyId);
      const index = rootId ? items.findIndex((it) => it.id === rootId) : -1;
      const rootItem = items[index];
      const root = rootItem && rootItem.kind !== "activity-group" ? rootItem.entry : undefined;
      if (!rootId || !root) return;
      setThreadExpanded(id, rootId, true);
      const collapse = useCommentCollapseStore.getState();
      if (collapse.isCollapsed(id, rootId)) collapse.toggle(id, rootId);
      if (!expandedResolved.has(rootId)) {
        const resolution = deriveThreadResolution(root, timelineView.threadReplies.get(rootId) ?? EMPTY_REPLIES);
        if (resolution.kind === "root" || (resolution.kind === "reply" && resolution.resolutionId !== replyId)) {
          toggleResolvedExpand(rootId, true);
        }
      }
      virtuosoRef.current?.scrollToIndex({ index, align: "start", offset: -16 });
      setPendingReplyJump({ replyId, rootId });
    },
    [id, items, replyToRoot, expandedResolved, timelineView.threadReplies, toggleResolvedExpand, setThreadExpanded],
  );
  // Land the pending reply once its row is in the DOM. The expansion commits
  // on the next render and Virtuoso mounts the thread a frame or two later, so
  // wait by frame (~1s cap), then re-align until async layout (markdown, code
  // highlight, images) settles. Drive scrollTop directly — never native
  // scrollIntoView (#3929) — and clear any sticky thread bar pinned at the top
  // of the viewport so it cannot cover the reply's header.
  useEffect(() => {
    const container = scrollContainerEl;
    if (!pendingReplyJump || !container) return;
    const { replyId, rootId } = pendingReplyJump;
    const index = items.findIndex(item => item.id === replyId);
    if (index < 0) return;
    virtuosoRef.current?.scrollToIndex({ index, align: "start", offset: -16 });
    let rafId = 0;
    let frames = 0;
    let last = -1;
    const align = () => {
      const el = document.getElementById(`comment-${replyId}`);
      if (!el) {
        if (++frames < 60) rafId = requestAnimationFrame(align);
        else setPendingReplyJump(null);
        return;
      }
      const stickyBar = document
        .getElementById(`comment-${rootId}`)
        ?.querySelector<HTMLElement>("[data-thread-sticky-bar]");
      const c = container.getBoundingClientRect();
      const e = el.getBoundingClientRect();
      const target = Math.max(
        0,
        container.scrollTop + (e.top - c.top) - 16 - (stickyBar?.offsetHeight ?? 0),
      );
      container.scrollTop = target;
      if (Math.abs(target - last) > 1 && ++frames < 90) {
        last = target;
        rafId = requestAnimationFrame(align);
        return;
      }
      flashJumpTarget(replyId);
      setPendingReplyJump(null);
    };
    rafId = requestAnimationFrame(align);
    return () => cancelAnimationFrame(rafId);
  }, [pendingReplyJump, scrollContainerEl, flashJumpTarget, items]);
  const jumpToMinimapTarget = useCallback(
    (commentId: string) =>
      replyToRoot.has(commentId) ? jumpToReply(commentId) : jumpToThread(commentId),
    [replyToRoot, jumpToReply, jumpToThread],
  );
  const jumpToComment = useCallback((commentId: string) => {
    const element = document.getElementById(`comment-${commentId}`);
    const container = scrollContainerEl;
    if (!element || !container) {
      jumpToMinimapTarget(commentId);
      return;
    }
    // Highlighting a reply force-opens its compact thread, which mounts the
    // hidden replies above the target after this scroll — so re-align until
    // the offset stops moving (same settle loop as the pending reply jump).
    flashJumpTarget(commentId);
    let frames = 0;
    let last = -1;
    const align = () => {
      const target = document.getElementById(`comment-${commentId}`) ?? element;
      const offset = target.getBoundingClientRect().top - container.getBoundingClientRect().top;
      container.scrollTop = Math.max(0, container.scrollTop + offset - 16);
      if (Math.abs(offset - last) > 1 && ++frames < 30) {
        last = offset;
        requestAnimationFrame(align);
      }
    };
    align();
  }, [scrollContainerEl, flashJumpTarget, jumpToMinimapTarget]);
  const [pendingAnnotationEdit, setPendingAnnotationEdit] = useState<{
    rootId: string; sourceCommentId: string; annotationId: string;
  } | null>(null);
  useEffect(() => { setPendingAnnotationEdit(null); }, [id]);
  const revealAnnotation = useCallback((rootId: string, annotation: ReplyAnnotation) => {
    const sourceCommentId = annotation.sourceCommentId;
    if (sourceCommentId !== rootId && replyToRoot.get(sourceCommentId) !== rootId) return false;
    if (!findDocuments.some(document => document.id === `comment-${sourceCommentId}`)) return false;
    setPendingAnnotationEdit({ rootId, sourceCommentId, annotationId: annotation.id });
    jumpToComment(sourceCommentId);
    return true;
  }, [replyToRoot, findDocuments, jumpToComment]);
  const completeAnnotationEdit = useCallback((annotationId: string) => {
    setPendingAnnotationEdit(current => current?.annotationId === annotationId ? null : current);
  }, []);
  // With comments that arrived while the reader was away, land on the first
  // of them so they read in order; otherwise go to the very end.
  const firstNewCommentId = newComments.ids[0];
  const clearNewComments = newComments.clear;
  const jumpLatest = jumpToLatest.jump;
  const onJumpToLatest = useCallback(() => {
    if (firstNewCommentId) {
      clearNewComments();
      jumpToMinimapTarget(firstNewCommentId);
    } else {
      jumpLatest();
    }
  }, [firstNewCommentId, clearNewComments, jumpToMinimapTarget, jumpLatest]);
  const jumpToLatestLabel = newComments.ids.length > 0
    ? t(($) => $.timeline.new_comments, { count: newComments.ids.length })
    : t(($) => $.timeline.jump_to_latest);
  // Header chip jump to a run's block. A run is either its own top-level row
  // or a slot inside the thread it answers, so find that row, undo whatever
  // folds the thread, scroll the row into place, then let the effect below
  // align the block itself once it mounts.
  const [pendingRunJump, setPendingRunJump] = useState<{ taskId: string; rootId: string; flashId: string } | null>(null);
  const jumpToRun = useCallback(
    (taskId: string) => {
      let index = items.findIndex((it) => it.kind === "run" && it.run.task.id === taskId);
      let run = index >= 0 ? (items[index] as Extract<TimelineItem, { kind: "run" }>).run : undefined;
      if (!run) {
        for (const [rootId, runs] of commentRuns) {
          run = runs.find((candidate) => candidate.task.id === taskId);
          if (run) {
            index = items.findIndex((it) => it.id === rootId);
            break;
          }
        }
      }
      const rootItem = items[index];
      if (!run || !rootItem) return;
      const root = rootItem.kind !== "activity-group" ? rootItem.entry : undefined;
      const headsThread = run.hasReply && run.commentId === rootItem.id;
      if (root && !headsThread) {
        setThreadExpanded(id, root.id, true);
        const collapse = useCommentCollapseStore.getState();
        if (collapse.isCollapsed(id, root.id)) collapse.toggle(id, root.id);
      }
      if (
        root && !expandedResolved.has(root.id) &&
        deriveThreadResolution(root, timelineView.threadReplies.get(root.id) ?? EMPTY_REPLIES).kind !== "none"
      ) {
        toggleResolvedExpand(root.id, true);
      }
      virtuosoRef.current?.scrollToIndex({ index, align: "start", offset: -16 });
      setPendingRunJump({ taskId, rootId: rootItem.id, flashId: run.hasReply && run.commentId ? run.commentId : taskId });
    },
    [id, items, commentRuns, expandedResolved, timelineView.threadReplies, toggleResolvedExpand, setThreadExpanded],
  );
  // Same frame-by-frame landing as a reply jump. A run that has replied may
  // only show as a log button in its comment's header, so align that comment.
  useEffect(() => {
    const container = scrollContainerEl;
    if (!pendingRunJump || !container) return;
    const { taskId, rootId, flashId } = pendingRunJump;
    const index = items.findIndex(item => item.id === flashId || (item.kind === "thread-row" && item.row.key === taskId));
    if (index >= 0) virtuosoRef.current?.scrollToIndex({ index, align: "start", offset: -16 });
    let rafId = 0;
    let frames = 0;
    let last = -1;
    const align = () => {
      const el = container.querySelector<HTMLElement>(`[data-run-slot-id="${taskId}"]`)
        ?? container.querySelector<HTMLElement>(`[data-run-id="${taskId}"]`)?.closest<HTMLElement>("[id^='comment-']");
      if (!el) {
        if (++frames < 60) rafId = requestAnimationFrame(align);
        else setPendingRunJump(null);
        return;
      }
      const stickyBar = document
        .getElementById(`comment-${rootId}`)
        ?.querySelector<HTMLElement>("[data-thread-sticky-bar]");
      const c = container.getBoundingClientRect();
      const e = el.getBoundingClientRect();
      const target = Math.max(
        0,
        container.scrollTop + (e.top - c.top) - 16 - (stickyBar && !el.contains(stickyBar) ? stickyBar.offsetHeight : 0),
      );
      container.scrollTop = target;
      if (Math.abs(target - last) > 1 && ++frames < 90) {
        last = target;
        rafId = requestAnimationFrame(align);
        return;
      }
      flashJumpTarget(flashId);
      setPendingRunJump(null);
    };
    rafId = requestAnimationFrame(align);
    return () => cancelAnimationFrame(rafId);
  }, [pendingRunJump, scrollContainerEl, flashJumpTarget, items]);

  const {
    reactions: issueReactions,
    toggleReaction: handleToggleIssueReaction,
  } = useIssueReactions(id, user?.id);

  const {
    subscribers, isSubscribed, subscriptionReason, subscriptionDelivery, subscriptionKnown,
    togglePending, subtreePending,
    toggleSubscribe: handleToggleSubscribe, toggleSubscriber, followAllActivity,
    unsubscribeFromSubtree: handleUnsubscribeSubtree,
  } = useIssueSubscribers(id, user?.id);

  // Attachments uploaded against this issue. Drives the description
  // editor's click-time fresh-sign download: NodeViews match
  // `src`/`href` against this list to resolve an attachment id before
  // calling `/api/attachments/{id}`.
  // The same list is the issue's complete file inventory: it labels each
  // comment file with its comment, independent of the bounded timeline.
  const { data: issueAttachments } = useQuery(issueAttachmentsOptions(id));
  const deliverableInventory = useMemo(
    () => deliverableInventoryFromAttachments(issueAttachments ?? []),
    [issueAttachments],
  );

  // Sub-issue queries
  const parentIssueId = issue?.parent_issue_id;
  const { data: parentIssue = null } = useQuery({
    ...issueDetailOptions(wsId, parentIssueId ?? ""),
    enabled: !!parentIssueId,
    initialData: () => allIssues.find((i) => i.id === parentIssueId),
  });

  // Project segment in the breadcrumb. The issue's project_id is the source of
  // truth — same URL renders the same breadcrumb regardless of entry path.
  const issueProjectId = issue?.project_id;
  const { data: breadcrumbProject = null } = useQuery({
    ...projectDetailOptions(wsId, issueProjectId ?? ""),
    enabled: !!issueProjectId,
  });
  const {
    data: childIssues = [],
    isSuccess: childIssuesLoaded,
    isFetching: childIssuesFetching,
  } = useQuery({
    ...childIssuesOptions(wsId, id),
    enabled: !!issue,
  });
  // Whether this issue has sub-issues, as opposed to "we have not looked yet".
  // childIssuesOptions sets refetchOnMount: "always", so a cached snapshot is
  // re-fetched on every mount and isSuccess alone still describes the previous
  // visit. The unsubscribe control below picks a different server write from
  // this answer, so a defaulted or stale empty array must not count as "no
  // sub-issues" (MUL-5714).
  const childCountKnown = childIssuesLoaded && !childIssuesFetching;
  // Parent's children — used to render the "x/y" progress next to the
  // "Sub-issue of …" breadcrumb under the title.
  const { data: parentChildIssues = [] } = useQuery({
    ...childIssuesOptions(wsId, parentIssueId ?? ""),
    enabled: !!parentIssueId,
  });
  // Workspace-wide parent→(done/total) map; lets each sub-issue row show its
  // OWN nested progress ring without opening it. Same query the list
  // surfaces use, so it's usually already cached.
  const { data: subIssueProgress } = useQuery({
    ...childIssueProgressOptions(wsId),
    enabled: childIssues.length > 0,
  });
  // User-level display preference for sub-issue rows (built-in field toggles
  // + opted-in workspace custom properties). `subIssueCustomProps` resolves
  // the persisted ids against this workspace's live, non-archived property
  // definitions — ids from other workspaces or archived properties drop out.
  const subIssueRowProps = useSubIssueDisplayStore((s) => s.rowProperties);
  const subIssueRowPropertyIds = useSubIssueDisplayStore((s) => s.rowPropertyIds);
  // Store-backed (not useState) so the collapsed state survives leaving the
  // issue and navigating back within the session.
  const subIssuesCollapsed = useSubIssuesCollapseStore((s) =>
    s.collapsedIssueIds.has(issueId),
  );
  const setSubIssuesCollapsedFor = useSubIssuesCollapseStore(
    (s) => s.setCollapsed,
  );
  const setSubIssuesCollapsed = useCallback(
    (update: boolean | ((prev: boolean) => boolean)) => {
      const prev = useSubIssuesCollapseStore
        .getState()
        .collapsedIssueIds.has(issueId);
      setSubIssuesCollapsedFor(
        issueId,
        typeof update === "function" ? update(prev) : update,
      );
    },
    [issueId, setSubIssuesCollapsedFor],
  );

  // The sub-issue list owns its selection, reset per issue. Rows, select-all
  // and the inline batch toolbar must read the same one: a peek renders this
  // inside a surface whose own selection provider would otherwise win for the
  // toolbar while the rows wrote elsewhere.
  const subIssueSelection = useCreateIssueSurfaceSelection(`sub-issues:${id}`);
  const {
    selectedIds,
    select: selectIds,
    deselect: deselectIds,
  } = subIssueSelection;

  const childIssueIds = useMemo(() => childIssues.map((c) => c.id), [childIssues]);
  const childSelectedCount = childIssueIds.filter((cid) =>
    selectedIds.has(cid),
  ).length;
  const allChildrenSelected =
    childIssueIds.length > 0 && childSelectedCount === childIssueIds.length;
  const someChildrenSelected = childSelectedCount > 0;
  const handleToggleSelectAllChildren = useCallback(() => {
    if (allChildrenSelected) deselectIds(childIssueIds);
    else selectIds(childIssueIds);
  }, [allChildrenSelected, childIssueIds, deselectIds, selectIds]);

  const loading = issueLoading;
  useEffect(() => {
    if (loading || isPeek || !wsId) return;
    const direction = useInboxSelectionStore.getState().consumeTransition(wsId, id);
    if (direction) setInboxTransition({ issueId: id, direction });
  }, [loading, isPeek, wsId, id]);

  // Deep-link landing. Semantically equivalent to navigating to
  // `#comment-${id}`: find the element with that id, scrollIntoView it.
  // Deep links keep the timeline virtualized. Mount the enclosing row by
  // index, then center and highlight the exact comment once its anchor exists.
  //
  // For a reply or threaded run inside a folded resolved thread, the target
  // is not in items (only the resolved-bar root is). Auto-expand the thread
  // first; the effect re-runs once items re-flatten.
  //
  // `scrollContainerEl` is in deps because the component early-returns a
  // loading skeleton while the issue query is pending. The scroll-container
  // ref populates only on the post-loading render, so it's the signal that
  // the timeline (and the deep-link target id) has actually rendered.
  useEffect(() => {
    if (!highlightCommentId || items.length === 0 || timelineLoading || commentTasksLoading) return;
    // An explicit replay request (re-click on the already-open notification
    // row): the host cleared the memento entry and bumped the token, so
    // re-arm the landing guard and fall through to the jump.
    if (lastHighlightRequestTokenRef.current !== highlightRequestToken) {
      lastHighlightRequestTokenRef.current = highlightRequestToken;
      didHighlightRef.current = null;
      // A replay asks a mounted, measured list to scroll: the page drives it.
      listOpenedAtRef.current = { listKey: `${wsId}:${id}`, commentId: null };
    }
    // A landing this effect's cleanup cut short resumes (see the cleanup).
    const resuming = interruptedLandingRef.current === highlightCommentId;
    if (!resuming && didHighlightRef.current === highlightCommentId) return;
    // The deep link already landed before this mount (memento entry — a tab
    // switch back or an in-tab return). The restored scroll offset is the
    // state the user left, and the jump must not fight it — but 01-DESIGN is
    // explicit that "memento restoration may suppress scrolling but must not
    // suppress revealing the target": a memento only proves THIS component
    // previously landed on the target, not that the target's containing
    // fold is still open now (a fresh mount may start fully collapsed, or an
    // intervening resolve/collapse may have refolded it since). So a
    // consumed memento skips the scroll/highlight/re-write below, but still
    // falls through to the same auto-expand branches a fresh landing uses.
    // A resumed landing and a deliverable jump are fresh landings, not repeats.
    const mementoConsumed = !resuming && !activeDeliverableJump && highlightConsumedRef.current;
    if (mementoConsumed) didHighlightRef.current = highlightCommentId;

    const targetId = highlightTargetId ?? highlightCommentId;
    const rootId = highlightRootId ?? targetId;
    const collapse = useCommentCollapseStore.getState();
    if (collapse.isCollapsed(id, rootId)) collapse.toggle(id, rootId);
    if (rootId) {
      // Root resolved → the whole thread is a folded bar.
      const rootItem = items[targetIdx];
      if (rootItem?.kind === "resolved-bar" || (rootItem?.kind === "run" && rootItem.entry?.resolved_at && !expandedResolved.has(rootId))) {
        toggleResolvedExpand(rootId, true);
        return;
      }
      // A reply is the resolution → the other replies fold behind the
      // "N comments" bar; expand if the target is one of those folded replies.
      if ((rootItem?.kind === "comment" || rootItem?.kind === "run") && rootItem.entry && !expandedResolved.has(rootId)) {
        const resolution = deriveThreadResolution(
          rootItem.entry,
          timelineView.threadReplies.get(rootId) ?? EMPTY_REPLIES,
        );
        if (resolution.kind === "reply" && resolution.resolutionId !== targetId) {
          toggleResolvedExpand(rootId, true);
          return;
        }
      }
    }

    // A consumed memento only ever suppresses the scroll/highlight/rewrite
    // below (01-DESIGN: "memento may suppress scrolling but never suppress
    // revealing the target") — the auto-expand branches above already ran
    // unconditionally, on every render, including this one, so the target's
    // fold gets (re-)opened even when this specific landing is a memento
    // replay rather than a fresh jump.
    if (mementoConsumed) return;

    // 01-DESIGN "Navigation resolves a loaded comment to its root and nested
    // fold ancestors, establishes a target pin, waits for the same committed
    // visibility signal, then scrolls/highlights. Only record didHighlight/
    // memento after the correct target DOM exists." The auto-expand branches
    // above already establish the pin (targetRootId/forceThreadOpen) and
    // request the resolved-thread expand; this is the gate that stops the
    // landing itself from running before that request's DOM has actually
    // committed. A resolved-thread expand just requested above hasn't
    // committed by definition, so this also naturally waits out that case
    // without a separate branch.
    // A standalone run has no thread to open: nothing is pinned, so there is
    // no reveal generation to wait for.
    if (targetRootId && !isDisclosureRevealCommitted(id, revealKey)) return;

    // A deleted reply renders nothing, so the id a notification or share link
    // carries has no anchor to scroll to and no row to flash. Land on the
    // comment above where it was; the id itself stays this landing's identity
    // for the guards and the memento below.
    const threadRootId = rootId ?? targetId;
    const landingId = commentLandingTarget(
      targetId,
      threadRootId,
      timelineView.threadReplies.get(threadRootId) ?? EMPTY_REPLIES,
    );
    const container = scrollContainerEl;
    if (!container || targetIdx < 0) return;
    const listOwnsScroll = listOpenedAtRef.current?.commentId === highlightCommentId;
    if (!listOwnsScroll && !document.getElementById(`comment-${landingId}`)) {
      virtuosoRef.current?.scrollToIndex({ index: targetIdx, align: "start", offset: -16 });
    }

    // Center the target comment WITHIN its own scroll container by driving the
    // container's scrollTop directly — never native scrollIntoView. Native
    // scrollIntoView is spec'd to scroll EVERY scrollable ancestor: on a cold
    // mount where the timeline is still growing (streaming agent), the inner
    // scroller can't satisfy centering on its own, so the scroll propagates up
    // and moves the desktop shell's `overflow:hidden` wrapper — shoving the
    // whole page, header included, off the top with no scrollbar to recover,
    // until a resize reflows it (#3929). Scoping the scroll to `container`
    // keeps it contained; re-centering across frames lands the comment
    // precisely once async heights (markdown, code highlight, streamed replies)
    // settle, instead of leaning on the ancestor scroll the way native did.
    let rafId = 0;
    let frames = 0;
    let last = -1;
    let landed = false;
    // Set once the re-centering loop has settled; a cleanup before that cuts
    // the landing short (see the cleanup).
    let settled = false;
    interruptedLandingRef.current = null;
    const center = () => {
      const el = document.getElementById(`comment-${landingId}`);
      if (!el) {
        if (!landed && ++frames < 120) rafId = requestAnimationFrame(center);
        return;
      }
      if (!landed) {
        landed = true;
        didHighlightRef.current = highlightCommentId;
        writeViewState(issueHighlightMementoKey(id), highlightCommentId);
        onHighlightLandedRef.current?.();
        // After the frame that paints the target, not inside it.
        requestAnimationFrame(() => window.setTimeout(releaseLandingChrome, 0));
        setHighlightedId(landingId);
        if (jumpFlashTimerRef.current !== null) window.clearTimeout(jumpFlashTimerRef.current);
        jumpFlashTimerRef.current = window.setTimeout(() => setHighlightedId(null), 2500);
      }
      if (listOwnsScroll) {
        settled = true;
        return;
      }
      const c = container.getBoundingClientRect();
      const e = el.getBoundingClientRect();
      const target = Math.max(
        0,
        container.scrollTop + (e.top - c.top) - Math.max(16, (container.clientHeight - e.height) / 2),
      );
      container.scrollTop = target;
      // Content is still laying out → the centered offset keeps shifting; keep
      // re-centering until it stabilizes (within 1px) or we hit ~0.5s of frames.
      if (Math.abs(target - last) > 1 && ++frames < 30) {
        last = target;
        rafId = requestAnimationFrame(center);
      } else {
        settled = true;
      }
    };
    rafId = requestAnimationFrame(center);

    return () => {
      cancelAnimationFrame(rafId);
      // A commit while the landing is still aligning (a realtime comment, a
      // run refresh, the refetch of a cached timeline) re-runs this effect and
      // cancels the loop here. Once the anchor has mounted, the guards above
      // count the landing as done, so mark it for the re-run to resume;
      // otherwise the viewport stays wherever the loop stopped, often the top
      // of the issue (MUL-9068).
      if (landed && !settled && didHighlightRef.current === highlightCommentId) {
        interruptedLandingRef.current = highlightCommentId;
      }
    };
  }, [highlightCommentId, highlightTargetId, highlightRequestToken, activeDeliverableJump, timelineLoading, commentTasksLoading, id, writeViewState, wsId, items, targetIdx, scrollContainerEl, highlightRootId, targetRootId, expandedResolved, timelineView, toggleResolvedExpand, isDisclosureRevealCommitted, revealKey, disclosureReveal.token, releaseLandingChrome]);

  const descEditorRef = useRef<ContentEditorRef>(null);
  const descriptionEditingRef = useRef(false);
  // Reactive twin of descriptionEditingRef: collapse-disable state must
  // re-render when focus enters/leaves the description, which a ref alone
  // cannot trigger.
  const [descriptionFocused, setDescriptionFocused] = useState(false);
  const { uploading: descUploading, onUploadingChange: onDescUploadingChange } =
    useUploadGate(descEditorRef);
  const descriptionExpanded = useIssueDisclosureStore(selectDescriptionExpanded(id));
  const setDescriptionExpanded = useIssueDisclosureStore((s) => s.setDescriptionExpanded);
  const effectiveDescriptionExpanded = descriptionExpanded || find.activeDocumentId === `find-description-${id}`;
  const descriptionSaveInFlightRef = useRef(false);
  const descriptionSaveIssueIdRef = useRef(id);
  const pendingDescriptionSaveRef = useRef<{
    markdown: string;
    baseMarkdown: string;
    attachmentIds: string[];
  } | null>(null);
  // Keep the description editor mounted from the start. Unlike the empty
  // composer shells, a long rendered description cannot swap between
  // react-markdown and ProseMirror without small per-block height differences
  // accumulating into a visible scroll/layout jump. The chunked Markdown path
  // keeps this single eager editor affordable; title and composers stay lazy.
  const titleEditorRef = useRef<TitleEditorRef>(null);
  const titleBaseRef = useRef<string | undefined>(issue?.title);
  // An unsaved title edit outlives the page and the editor; it is cleared only once the server accepts it.
  const [titleDraft, setTitleDraft, clearTitleDraft] = usePersistedDraft(useIssueTitleDraftStore, id, EMPTY_TEXT_EDIT);
  const [titleConflictDraft, setTitleConflictDraft] = useState<string | null>(null);
  // While a title save is in flight the cache already shows the new title, and a failure rolls it back.
  // Matching `issue.title` then proves nothing, so the draft is kept until the save settles.
  const titleSavesInFlightRef = useRef(0);
  // Bumped when a conflicting title draft is discarded. TitleEditor reads its
  // text from `defaultValue` at mount and exposes no imperative setter, so
  // remounting is the only way to put the server's title back in the editor.
  const [titleResetToken, setTitleResetToken] = useState(0);
  // A drop/insert into a collapsed description must land in the real,
  // visible editor — never insert into the inert clipped one — per
  // 01-DESIGN.md's expand-before-insert contract. Setting the store flag and
  // calling `uploadFile` in the same handler is not enough: the state change
  // is synchronous but the subscribed DOM update (clearing `aria-hidden`/
  // `inert`) is batched until this handler returns, so the real editor can
  // still be inert at the moment of insert. Queue the file and drain it from
  // an effect that runs after the expanded render commits instead.
  const descPendingUploadsRef = useRef<File[]>([]);
  useEffect(() => {
    setTitleConflictDraft(null);
    titleSavesInFlightRef.current = 0;
    // A restored edit keeps the title it started from, so a title that moved meanwhile still conflicts.
    titleBaseRef.current = useIssueTitleDraftStore.getState().getDraft(id)?.base ?? undefined;
    descriptionSaveInFlightRef.current = false;
    descriptionSaveIssueIdRef.current = id;
    pendingDescriptionSaveRef.current = null;
    descriptionEditingRef.current = false;
    descPendingUploadsRef.current = [];
    setDescriptionFocused(false);
  }, [id]);
  const titleLazy = useLazyEditor({ editorRef: titleEditorRef, resetKey: id, initialActive: titleDraft.value !== null });
  const uploadIntoExpandedDescription = useCallback(
    (file: File) => {
      if (descriptionExpanded) {
        descEditorRef.current?.uploadFile(file);
        return;
      }
      descPendingUploadsRef.current.push(file);
      setDescriptionExpanded(id, true);
    },
    [id, descriptionExpanded, setDescriptionExpanded],
  );
  useEffect(() => {
    if (!descriptionExpanded) return;
    const pending = descPendingUploadsRef.current;
    if (pending.length === 0) return;
    descPendingUploadsRef.current = [];
    for (const file of pending) descEditorRef.current?.uploadFile(file);
  }, [descriptionExpanded]);
  const { isDragOver: descDragOver, dropZoneProps: descDropZoneProps } = useFileDropZone({
    onDrop: (files) => files.forEach((file) => uploadIntoExpandedDescription(file)),
  });
  // Pending uploads in the description editor. We don't pass `issueId` on
  // upload (to avoid orphaning attachments when the user deletes the file
  // from the markdown), so they start unattached and we re-bind them via
  // `attachment_ids` on the next description save. Drives editor previews
  // so text/code attachments show an Eye before the bind round-trips.
  const [descPendingAttachments, setDescPendingAttachments] = useState<Attachment[]>([]);
  const descPendingAttachmentsRef = useRef<Attachment[]>([]);
  // Memoized because the image sequence below keys off this array: without it
  // the concat branch hands a fresh reference to every render and rebuilds the
  // whole issue's sequence on each one.
  const descEditorAttachments = useMemo(
    () =>
      descPendingAttachments.length > 0
        ? [...(issueAttachments ?? []), ...descPendingAttachments]
        : issueAttachments,
    [issueAttachments, descPendingAttachments],
  );

  const chainedAttachments = useMemo(() => chainedIssueAttachments(issueAttachments), [issueAttachments]);

  // Every previewable file in this issue, in the order the page renders them:
  // the description first, then each timeline comment with its thread replies
  // nested under it (MUL-5752). Built from `items` rather than the flat
  // timeline so a reply sits next to the root it renders under, and from data
  // rather than the DOM because Virtuoso only mounts the visible window.
  //
  // A resolved thread that is currently collapsed still contributes its
  // files: they belong to the issue and are one click from being on screen,
  // so leaving them out would make the counter change depending on which
  // threads happen to be folded.
  //
  // The description renders no standalone cards, and its attachment list is
  // the whole issue's (comment uploads keep `issue_id`) — it only resolves the
  // description's own references, or every comment file would be counted at
  // the description's position.
  const deliverableSources = useMemo(
    () => mergeDeliverableSources(deliverableInventory, timeline),
    [deliverableInventory, timeline],
  );
  const previewSequence = useMemo(() => {
    const blocks: ImageSequenceBlock[] = [
      {
        id: DESCRIPTION_BLOCK_ID,
        content: issue?.description,
        attachments: descEditorAttachments,
        standalone: false,
      },
    ];
    for (const entry of deliverableSources) {
      blocks.push({ id: entry.id, content: entry.content, attachments: entry.attachments });
    }
    return collectPreviewSequence(blocks);
  }, [issue?.description, descEditorAttachments, deliverableSources]);

  // The files this issue has delivered as a whole: the sidebar section, the
  // overview grid, the viewer's info panel and the comments' version badges
  // all read this (MUL-7649).
  const deliverableFiles = useMemo(() => collectDeliverableFiles(deliverableSources), [deliverableSources]);
  const commentById = useMemo(
    () =>
      new Map(
        deliverableSources
          .filter((entry) => entry.type === "comment")
          .map((entry) => [entry.id, entry] as const),
      ),
    [deliverableSources],
  );

  // File origins use the deep-link landing path: load missing thread context,
  // unfold it and wait for virtual rows and their measured heights to settle.
  const locateOrigin = useCallback(
    (origin: DeliverableOrigin) => {
      if (origin.kind === "description") {
        scrollContainerEl?.scrollTo({ top: 0, behavior: "smooth" });
        return;
      }
      setDeliverableJump((previous) => ({
        issueId: id,
        linkedCommentId,
        linkedRequestToken,
        commentId: origin.commentId,
        token: Math.max(previous?.token ?? 0, linkedRequestToken ?? 0) + 1,
      }));
    },
    [scrollContainerEl, id, linkedCommentId, linkedRequestToken],
  );

  // Identifier form, like the issue's own copy link: a pasted link should say
  // which issue it points at.
  const copyDeliverableLink = useCopyDeliverableLink(issueRouteRef({ id, identifier: issue?.identifier }));
  const describeDeliverable = useDeliverableDetails({
    files: deliverableFiles,
    commentById,
    onLocate: locateOrigin,
    onCopyLink: copyDeliverableLink,
  });
  const deliverableLinkRead = useFreshTimelineRead(id, openDeliverable, timelineUpdatedAt);
  // A `/deliverables/<name>` link that opened the viewer gives the address
  // back once the reader is done with the file, through the viewer and the
  // overview alike. Viewer sessions the reader opens themselves leave the
  // address alone.
  const deliverableLink = useDeliverableLinkSession(onDeliverableLinkDone);
  const onDeliverableLinkDoneStable = useCallback(() => {
    onDeliverableLinkDone?.();
  }, [onDeliverableLinkDone]);
  // The overview remembers the file the viewer was showing, so `G` can go
  // back to it.
  const [overview, setOverview] = useState<{ open: boolean; returnKey: string | null }>({
    open: false,
    returnKey: null,
  });
  useEffect(() => {
    setOverview({ open: false, returnKey: null });
  }, [id]);
  const openOverview = useCallback(() => setOverview({ open: true, returnKey: null }), []);
  const { onOverviewOpenedFromViewer, onOverviewClosed } = deliverableLink;
  const openOverviewFromViewer = useCallback(
    (key: string) => {
      onOverviewOpenedFromViewer();
      setOverview({ open: true, returnKey: key });
    },
    [onOverviewOpenedFromViewer],
  );
  const closeOverview = useCallback(() => {
    setOverview((current) => ({ ...current, open: false }));
    onOverviewClosed();
  }, [onOverviewClosed]);

  const handleDescriptionUpload = useCallback(
    async (file: File) => {
      const result = await uploadWithToast(file);
      if (result) {
        descPendingAttachmentsRef.current = [
          ...descPendingAttachmentsRef.current,
          result,
        ];
        setDescPendingAttachments(descPendingAttachmentsRef.current);
      }
      return result;
    },
    [uploadWithToast],
  );

  useEffect(() => {
    descPendingAttachmentsRef.current = [];
    setDescPendingAttachments([]);
  }, [id]);

  // With live editing on, the description is a shared document that the
  // collaboration service saves. The solo editor still shows first, so the
  // page never waits on that answer and works as before when live editing is
  // off; the live editor takes over once it is ready, though not while
  // someone is in the solo one. Unmounting the solo editor flushes its
  // pending save, which the server merges into the live document.
  const descCollab = useDocCollab(id, issue?.description ?? "", "issue", !guest);
  const descLive = descCollab.mode === "live" ? descCollab : null;
  const [descLiveReadyFor, setDescLiveReadyFor] = useState<number | null>(null);
  const [descSoloHeld, setDescSoloHeld] = useState(false);
  useEffect(() => setDescSoloHeld(false), [id]);
  const showLiveDesc = !!descLive?.ready && descLiveReadyFor === descLive.generation && !descSoloHeld;
  const descContentRef = useRef<HTMLDivElement>(null);
  const descPresence = useMemo(
    () => descLive?.collaborators.filter((c) => !(c.kind === "member" && c.id === user?.id)) ?? [],
    [descLive?.collaborators, user?.id],
  );
  const jumpToCollaborator = useCallback((c: DocCollaborator) => {
    const caret = descContentRef.current?.querySelector<HTMLElement>(
      `.collaboration-carets__caret[data-collaborator="${CSS.escape(`${c.kind}:${c.id}`)}"]`,
    );
    caret?.scrollIntoView({ block: "center", behavior: "smooth" });
  }, []);
  const descCarried = descCollab.mode === "solo" ? (descCollab.carried ?? null) : null;
  const queueDescriptionSaveRef = useRef<
    ((draft: { markdown: string; baseMarkdown: string; attachmentIds: string[] }) => void) | null
  >(null);
  useEffect(() => {
    // Typed into a live session that never connected: save it as a solo edit.
    if (descCarried === null) return;
    queueDescriptionSaveRef.current?.({
      markdown: descCarried,
      baseMarkdown: issue?.description ?? "",
      attachmentIds: descPendingAttachmentsRef.current
        .filter((a) => contentReferencesAttachment(descCarried, a))
        .map((a) => a.id),
    });
    // Once per carried draft; the issue is read as it is then.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [descCarried]);

  // Shared issue actions (mutations, pin, copy-link, modal dispatch, etc.).
  // Called before the `if (!issue)` early return so hook order stays stable.
  const actions = useIssueActions(issue);
  const handleUpdateField = actions.updateField;
  // The Inbox page owns archive and restore for the notification it has open.
  const archivedInboxItemId = useArchivedInboxItemId(id, !inbox);
  const pageMenu = useIssuePageMenu();

  // Labels live in their own query (not on the issue body) — fetch the count
  // here so seeding can decide whether the "Labels" optional row should be
  // shown for an issue that already has labels attached.
  const { data: attachedLabels = [] } = useQuery(issueLabelsOptions(wsId, id));
  const attachedLabelsCount = attachedLabels.length;

  // Custom property catalog. Includes archived definitions: an issue can
  // still carry a value written before the archive, and that row must stay
  // renderable (read-only) until someone clears it.
  const { data: workspaceProperties = [] } = useQuery(propertyListOptions(wsId, true));

  // Sub-issue rows only surface live definitions: the display picker offers
  // non-archived properties, and chips render only ids that resolve here.
  const activeWorkspaceProperties = useMemo(
    () => workspaceProperties.filter((p) => p.archived !== true),
    [workspaceProperties],
  );
  const subIssueCustomProps = useMemo(
    () =>
      subIssueRowPropertyIds
        .map((pid) => activeWorkspaceProperties.find((p) => p.id === pid))
        .filter((p): p is IssueProperty => !!p),
    [subIssueRowPropertyIds, activeWorkspaceProperties],
  );

  // Seed the visible-optional-props set:
  //   - on issue switch, reset to whichever fields are currently set
  //   - on the SAME issue, additively pick up fields the user just set
  //     (so the row stays visible after they edit + clear in one session)
  // Removal happens only on issue switch — never on clear.
  const seededIssueIdRef = useRef<string | null>(null);
  useEffect(() => {
    if (!issue) return;
    if (seededIssueIdRef.current !== issue.id) {
      seededIssueIdRef.current = issue.id;
      setAutoOpenProp(null);
      setAutoOpenCustomProp(null);
      const seed = new Set<OptionalPropKey>();
      for (const k of OPTIONAL_PROP_KEYS) {
        if (isOptionalPropSet(issue, k, attachedLabelsCount)) seed.add(k);
      }
      setVisibleOptionalProps(seed);
      setVisibleCustomProps(new Set(Object.keys(issue.properties ?? {})));
      return;
    }
    setVisibleOptionalProps((prev) => {
      let next = prev;
      for (const k of OPTIONAL_PROP_KEYS) {
        if (isOptionalPropSet(issue, k, attachedLabelsCount) && !next.has(k)) {
          if (next === prev) next = new Set(prev);
          next.add(k);
        }
      }
      return next;
    });
    setVisibleCustomProps((prev) => {
      let next = prev;
      for (const propertyId of Object.keys(issue.properties ?? {})) {
        if (!next.has(propertyId)) {
          if (next === prev) next = new Set(prev);
          next.add(propertyId);
        }
      }
      return next;
    });
  }, [issue, attachedLabelsCount]);

  const addOptionalProp = useCallback(
    (key: OptionalPropKey) => {
      setVisibleOptionalProps((prev) => {
        if (prev.has(key)) return prev;
        const next = new Set(prev);
        next.add(key);
        return next;
      });
      setAutoOpenProp(key);
      // Dismiss the "+ Add property" popover so it doesn't sit stacked
      // behind the picker we're about to auto-open.
      setAddPropPopoverOpen(false);
    },
    [],
  );

  const addCustomProp = useCallback((propertyId: string) => {
    setVisibleCustomProps((prev) => {
      if (prev.has(propertyId)) return prev;
      const next = new Set(prev);
      next.add(propertyId);
      return next;
    });
    setAutoOpenCustomProp(propertyId);
    setAddPropPopoverOpen(false);
  }, []);

  // Clear the auto-open flag after the next render so pickers (which read
  // `defaultOpen` once via a useState initializer) keep the open state they
  // captured on mount, but later interactions don't re-trigger it.
  useEffect(() => {
    if (autoOpenProp === null) return;
    setAutoOpenProp(null);
  }, [autoOpenProp]);

  useEffect(() => {
    if (autoOpenCustomProp === null) return;
    setAutoOpenCustomProp(null);
  }, [autoOpenCustomProp]);

  const handleToggleSidebar = useCallback(() => {
    if (isMobile) {
      setMobileSidebarOpen((open) => !open);
      return;
    }

    const panel = sidebarRef.current;
    if (!panel) return;
    const nextOpen = panel.isCollapsed();
    beginDesktopSidebarToggle(nextOpen);
    window.requestAnimationFrame(() => {
      if (nextOpen) panel.expand();
      else panel.collapse();
    });
  }, [beginDesktopSidebarToggle, isMobile, sidebarRef]);

  // The header's wakeup chip opens the sidebar and brings the Wakeups section
  // into view with focus on its heading (CHE-1727). Upstream's sidebar no
  // longer force-shows a section the reader hid, so this only reaches a
  // section that is rendered.
  const openWakeups = useCallback(() => {
    if (!sidebarOpen) handleToggleSidebar();
    window.requestAnimationFrame(() => {
      const heading = document.getElementById(`issue-wakeups-${id}`);
      heading?.scrollIntoView({ block: "nearest" });
      heading?.focus({ preventScroll: true });
    });
  }, [handleToggleSidebar, id, sidebarOpen]);

  // `?focus=wakeups` lands on the same section as the header chip, once the
  // issue has loaded and the section exists (CHE-3397).
  useWakeupsFocus(id, openWakeups, !loading && !!issue);

  useRightSidebarShortcut(rightSidebarShortcutTargetRef, handleToggleSidebar);

  const scrollAnchor = useIssueDetailScrollAnchor({
    issueId: id,
    scrollContainerEl,
    ready: !!issue && !loading && !timelineLoading,
    disabled: isPeek,
    keyAt: (index) => (items[index] ? timelineItemKey(items[index]) : undefined),
  });
  const scrollAnchorIndex = scrollAnchor ? items.findIndex((item) => timelineItemKey(item) === scrollAnchor.key) : -1;
  // A deep link not yet landed opens the list AT its comment. Asking a mounted
  // list to scroll there instead lands it on a guess and then on NaN, which
  // snaps the page to the top (CHE-5376).
  const openAtLandingIndex = highlightCommentId && !highlightConsumed ? targetIdx : -1;
  const listKey = `${wsId}:${id}`;
  if (!loading && !!scrollContainerEl && !(timelineLoading && timelineView.groups.length === 0) && listOpenedAtRef.current?.listKey !== listKey) {
    listOpenedAtRef.current = { listKey, commentId: openAtLandingIndex >= 0 ? highlightCommentId ?? null : null };
  }

  const landingPending = !!highlightCommentId && !highlightConsumed;
  // The timeline is fully loaded and has no row for the target (deleted
  // comment, stale link): the landing has nothing to own, so the previous
  // reading position is restored like any other return.
  const landingTargetMissing = landingPending && !timelineLoading && !commentTasksLoading && targetIdx < 0;

  useIssueDetailScrollRestore({
    restoreKey: `${wsId}:${id}`,
    scrollContainerEl,
    ready: !!issue && !loading && !timelineLoading,
    // Comment landings and semantic reading anchors own their scroll. Pixel
    // restoration still covers positions outside the discussion (description,
    // sub-issues) and adapters without a saved timeline anchor.
    // A peek always opens at the top: it is a glance, not a place to return to.
    // The timeline anchor owns the restore when one resolves: a pixel offset
    // lands on a different comment once virtualized row heights are measured.
    disabled: isPeek || scrollAnchorIndex >= 0 || (landingPending && !landingTargetMissing),
    // The tab memento's offset, when the platform serves one. It must win
    // over this hook's module-level map — see the parameter doc.
    overrideTop: restoredScrollTop,
  });

  const issueClosed = !!issue && ["done", "closed"].includes(resolveStatusCategory(issue.status));
  const sidebarAddables = useIssueSidebarAddables({
    issueId: id,
    closed: issueClosed,
    enabled: !!issue && !guest,
    pullRequestsAvailable: githubSettings.prSidebar,
  });
  const endSidebarReveal = useCallback(() => setSidebarReveal(null), []);

  // Keep the identity value stable across unrelated issue-detail updates so
  // context consumers only re-render when the owning issue changes identity.
  const resolvedIssueId = issue?.id;
  const resolvedIssueIdentifier = issue?.identifier;
  const currentIssueRenderContext = useMemo(
    () =>
      resolvedIssueId && resolvedIssueIdentifier
        ? { id: resolvedIssueId, identifier: resolvedIssueIdentifier }
        : null,
    [resolvedIssueId, resolvedIssueIdentifier],
  );

  if (issueReadState.loadFailed) {
    return (
      <IssueLoadError
        showBackLink={!onDelete}
        leading={leadingAction}
        trailing={trailingActions}
        onRetry={issueReadState.retry}
        isRetrying={issueReadState.isRetrying}
      />
    );
  }

  if (loading) {
    // The host's trailing controls hold the peek's only close button, so the
    // waiting and missing states carry them too.
    return (
      <IssueDetailSkeleton
        leading={leadingAction}
        trailing={trailingActions}
        sidebar={!isPeek && desktopSidebarOpen}
        layoutId={layoutId}
        inboxItem={inbox?.item}
      />
    );
  }

  if (!issue) {
    return (
      <IssueNotFound
        showBackLink={!onDelete}
        leading={leadingAction}
        trailing={trailingActions}
      />
    );
  }

  const requesterName =
    issue.creator_type === "agent" && issue.requester_id
      ? getActorName("member", issue.requester_id)
      : undefined;

  const persistDescriptionSave = (
    draft: { markdown: string; baseMarkdown: string; attachmentIds: string[] },
  ) => {
    descriptionSaveInFlightRef.current = true;
    handleUpdateField(
      {
        description: draft.markdown,
        description_base: draft.baseMarkdown,
        attachment_ids:
          draft.attachmentIds.length > 0 ? draft.attachmentIds : undefined,
      },
      {
        onSuccess: (serverIssue) => {
          if (descriptionSaveIssueIdRef.current !== id) return;
          descriptionSaveInFlightRef.current = false;
          // These ids are now bound server-side (attachment_ids above landed).
          // Drop them from the locally-tracked pending set so collapse only
          // stays refused while a bind is actually outstanding, not for the
          // rest of the issue visit (the design permits refusal while
          // upload/bind is pending, not forever).
          if (draft.attachmentIds.length > 0) {
            const bound = new Set(draft.attachmentIds);
            descPendingAttachmentsRef.current = descPendingAttachmentsRef.current.filter(
              (a) => !bound.has(a.id),
            );
            setDescPendingAttachments(descPendingAttachmentsRef.current);
          }
          const pending = pendingDescriptionSaveRef.current;
          pendingDescriptionSaveRef.current = null;
          if (pending) {
            // Usually the accepted document is exactly what we submitted. If
            // the server appended late channel media, keep the submitted body
            // as the next editor baseline: the server can recognize that the
            // only delta is media the editor never saw and preserve it again.
            const nextBase = serverIssue.description === draft.markdown
              ? serverIssue.description
              : draft.markdown;
            persistDescriptionSave({ ...pending, baseMarkdown: nextBase });
          }
        },
        onError: () => {
          if (descriptionSaveIssueIdRef.current !== id) return;
          descriptionSaveInFlightRef.current = false;
          pendingDescriptionSaveRef.current = null;
        },
      },
    );
  };

  const queueDescriptionSave = (
    draft: { markdown: string; baseMarkdown: string; attachmentIds: string[] },
  ) => {
    if (descriptionSaveInFlightRef.current) {
      pendingDescriptionSaveRef.current = draft;
      return;
    }
    persistDescriptionSave(draft);
  };
  queueDescriptionSaveRef.current = queueDescriptionSave;

  const canAddProperty = OPTIONAL_PROP_KEYS.some(
    (key) => !visibleOptionalProps.has(key) && (key !== "stage" || issue.parent_issue_id != null),
  ) || workspaceProperties.some(
    (property) => !property.archived && !visibleCustomProps.has(property.id) && issue.properties?.[property.id] === undefined,
  );
  // "+ Add property" — an inline row at the bottom of the Properties grid
  // that opens a Popover listing optional fields not yet displayed. Hidden
  // once every optional field is on screen.
  const addPropertyRow = canAddProperty && (
    <div className="col-span-2 mt-1">
      <Popover open={addPropPopoverOpen} onOpenChange={setAddPropPopoverOpen}>
        <PopoverTrigger
          className="-mx-2 flex items-center gap-2.5 rounded-md px-2 py-1 text-label text-muted-foreground transition-colors hover:bg-accent/50 hover:text-foreground"
        >
          <Plus className="size-3.5 shrink-0" />
          <span>{t(($) => $.detail.add_property_action)}</span>
        </PopoverTrigger>
        {/* Each option leads with the icon the resulting picker uses, so the
            list reads as a preview of what will show up above. 388px is the
            sidebar's 420px resize limit minus its 16px padding on each side.
            Popover (unlike DropdownMenu) has no height bound of its own, so a
            long property catalog must be capped to the space the positioner
            reports and scroll inside, or its tail renders off-screen. */}
        <PopoverContent
          align="start"
          data-testid="add-property-popover"
          className="max-h-(--available-height) w-max min-w-44 max-w-[min(388px,var(--available-width))] overflow-y-auto overscroll-contain p-1"
        >
          {OPTIONAL_PROP_KEYS.filter((k) => !visibleOptionalProps.has(k) && (k !== "stage" || issue.parent_issue_id != null)).map((k) => (
            <button
              key={k}
              type="button"
              onClick={() => addOptionalProp(k)}
              className="flex w-full items-center gap-2 rounded-md px-2 py-1 text-left text-caption text-foreground transition-colors hover:bg-accent focus-visible:bg-accent focus-visible:outline-none"
            >
              {k === "project" && (
                <FolderKanban className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
              )}
              {k === "priority" && (
                <PriorityIcon priority="medium" inheritColor className="text-muted-foreground" />
              )}
              {k === "stage" && (
                <Milestone className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
              )}
              {k === "start_date" && (
                <CalendarClock className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
              )}
              {k === "due_date" && (
                <CalendarDays className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
              )}
              {k === "labels" && (
                <Tag className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
              )}
              <span className="truncate">
                {k === "project" && t(($) => $.detail.prop_project)}
                {k === "priority" && t(($) => $.detail.prop_priority)}
                {k === "stage" && t(($) => $.detail.prop_stage)}
                {k === "start_date" && t(($) => $.detail.prop_start_date)}
                {k === "due_date" && t(($) => $.detail.prop_due_date)}
                {k === "labels" && t(($) => $.detail.prop_labels)}
              </span>
            </button>
          ))}
          {(() => {
            const addable = workspaceProperties.filter(
              (p) =>
                !p.archived &&
                !visibleCustomProps.has(p.id) &&
                issue.properties?.[p.id] === undefined,
            );
            if (addable.length === 0) return null;
            return (
              <>
                <div className="my-1 h-px bg-border" />
                {addable.map((p) => (
                  <TruncatedTooltip key={p.id} content={p.name}>
                    <button
                      type="button"
                      onClick={() => addCustomProp(p.id)}
                      className="flex w-full items-center gap-2 rounded-md px-2 py-1 text-left text-caption text-foreground transition-colors hover:bg-accent focus-visible:bg-accent focus-visible:outline-none"
                    >
                      {p.icon ? (
                        <PropertyIcon property={p} className="size-3.5 text-caption" />
                      ) : (
                        <SlidersHorizontal className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                      )}
                      <span className="line-clamp-2 min-w-0 break-words [overflow-wrap:anywhere]">{p.name}</span>
                    </button>
                  </TruncatedTooltip>
                ))}
              </>
            );
          })()}
        </PopoverContent>
      </Popover>
    </div>
  );

  const builtInPropLabels = [
    t(($) => $.detail.prop_status),
    t(($) => $.detail.prop_assignee),
    t(($) => $.detail.prop_project),
    t(($) => $.workflows.project.label),
    t(($) => $.detail.prop_priority),
    t(($) => $.detail.prop_stage),
    t(($) => $.detail.prop_start_date),
    t(($) => $.detail.prop_due_date),
    t(($) => $.detail.prop_labels),
  ];
  const locked = (value: ReactNode) => (
    <>
      <div inert className="flex min-w-0 flex-1 items-center">{value}</div>
      <Lock className="size-3 shrink-0 text-faint-foreground" aria-hidden="true" />
    </>
  );
  const guestProperties = (
    <div>
      <IssueSidebarSectionHeader title={t(($) => $.detail.section_properties)} open={propertiesOpen} onOpenChange={setPropertiesOpen} />
      <IssueSidebarCollapsible open={propertiesOpen}>
        <IssueSidebarPropertyGrid sizerLabels={builtInPropLabels}>
          <PropRow interactive={false} label={t(($) => $.detail.prop_status)}>
            {locked(<StatusPicker status={issue.status} onUpdate={handleUpdateField} align="start" projectId={issue.project_id} issue={issue} />)}
          </PropRow>
          <PropRow interactive={false} label={t(($) => $.detail.prop_assignee)}>
            {locked(<AssigneePicker assigneeType={issue.assignee_type} assigneeId={issue.assignee_id} onUpdate={handleUpdateField} align="start" />)}
          </PropRow>
          <PropRow interactive={false} label={t(($) => $.detail.prop_priority)}>
            {locked(<PriorityPicker priority={issue.priority} onUpdate={handleUpdateField} align="start" />)}
          </PropRow>
          {issue.project_id && (
            <PropRow interactive={false} label={t(($) => $.detail.prop_project)}>
              {locked(<IssueProjectPicker projectId={issue.project_id} onUpdate={handleUpdateField} issue={issue} />)}
            </PropRow>
          )}
          {attachedLabels.length > 0 && (
            <PropRow interactive={false} label={t(($) => $.detail.prop_labels)}>
              {locked(<LabelPicker issueId={issue.id} align="start" />)}
            </PropRow>
          )}
        </IssueSidebarPropertyGrid>
        <p className="px-4 pt-2 text-caption text-muted-foreground">{t(($) => $.detail.guest_readonly_note)}</p>
      </IssueSidebarCollapsible>
    </div>
  );

  const sidebarSections: Record<IssueSidebarSection, ReactNode> = {
    properties: guest ? guestProperties : (
      <div>
        <IssueSidebarSectionHeader title={t(($) => $.detail.section_properties)} open={propertiesOpen} onOpenChange={setPropertiesOpen}  />
        <IssueSidebarCollapsible open={propertiesOpen}><IssueSidebarPropertyGrid sizerLabels={builtInPropLabels}>
          {/* Status and assignee — always rendered. */}
          <PropRow label={t(($) => $.detail.prop_status)}>
            <StatusPicker
              status={issue.status}
              onUpdate={handleUpdateField}
              align="start"
              projectId={issue.project_id}
              workflowId={issue.workflow_id}
              issue={issue}
              onMarkDuplicate={actions.openMarkDuplicate}
              isDuplicate={isDuplicateIssue(issue)}
            />
          </PropRow>
          <PropRow label={t(($) => $.detail.prop_assignee)}>
            <AssigneePicker assigneeType={issue.assignee_type} assigneeId={issue.assignee_id} onUpdate={handleUpdateField} align="start" />
          </PropRow>
          {visibleOptionalProps.has("project") && (
            <PropRow label={t(($) => $.detail.prop_project)}>
              <IssueProjectPicker
                projectId={issue.project_id}
                onUpdate={handleUpdateField}
                issue={issue}
                defaultOpen={autoOpenProp === "project"}
              />
            </PropRow>
          )}
          <IssueWorkflowPropRow issueId={issue.id} projectId={issue.project_id} workflowId={issue.workflow_id} status={issue.status} label={t(($) => $.workflows.project.label)} />

          {/* Optional props — rendered only when set on the issue OR added
              via "+ Add property" in this session. Row order follows the
              order of `OPTIONAL_PROP_KEYS`. */}
          {visibleOptionalProps.has("priority") && (
            <PropRow label={t(($) => $.detail.prop_priority)}>
              <PriorityPicker
                priority={issue.priority}
                onUpdate={handleUpdateField}
                align="start"
                defaultOpen={autoOpenProp === "priority"}
              />
            </PropRow>
          )}
          {issue.parent_issue_id != null && visibleOptionalProps.has("stage") && (
            <PropRow label={t(($) => $.detail.prop_stage)}>
              <StagePicker
                stage={issue.stage}
                onUpdate={handleUpdateField}
                maxStage={maxSiblingStage(parentChildIssues)}
                align="start"
                defaultOpen={autoOpenProp === "stage"}
              />
            </PropRow>
          )}
          {visibleOptionalProps.has("start_date") && (
            <PropRow label={t(($) => $.detail.prop_start_date)}>
              <StartDatePicker
                startDate={issue.start_date}
                onUpdate={handleUpdateField}
                defaultOpen={autoOpenProp === "start_date"}
              />
            </PropRow>
          )}
          {visibleOptionalProps.has("due_date") && (
            <PropRow label={t(($) => $.detail.prop_due_date)}>
              <DueDatePicker
                dueDate={issue.due_date}
                onUpdate={handleUpdateField}
                defaultOpen={autoOpenProp === "due_date"}
              />
            </PropRow>
          )}
          {visibleOptionalProps.has("labels") && (
            <PropRow label={t(($) => $.detail.prop_labels)}>
              <LabelPicker
                issueId={issue.id}
                align="start"
                defaultOpen={autoOpenProp === "labels"}
              />
            </PropRow>
          )}

          {/* Custom properties — same progressive disclosure as the
              built-in optional props: a row renders when the issue has a
              value OR the user added the property this session. Archived
              definitions render read-only until their value is cleared. */}
          {workspaceProperties
            .filter(
              (p) =>
                issue.properties?.[p.id] !== undefined ||
                (!p.archived && visibleCustomProps.has(p.id)),
            )
            .map((p) => (
              <PropRow
                key={p.id}
                label={
                  <>
                    <PropertyIcon property={p} className="size-3.5 text-caption" />
                    <TruncatedTooltip content={p.name} focusable>
                      <span className="line-clamp-2 min-w-0 break-words [overflow-wrap:anywhere]">{p.name}</span>
                    </TruncatedTooltip>
                  </>
                }
              >
                <CustomPropertyValueEditor
                  issue={issue}
                  property={p}
                  defaultOpen={autoOpenCustomProp === p.id}
                />
              </PropRow>
            ))}

          {/* Fork rows the v0.13.0 footer does not carry: who asked for the
              issue (requested_by_id) and which surface created it (CHE-6098). */}
          {issue.requested_by_id && (
            <PropRow label={t(($) => $.detail.prop_requested_by)}>
              <ActorAvatar actorType="member" actorId={issue.requested_by_id} size="sm" enableHoverCard />
              <span className="cursor-pointer truncate">{getActorName("member", issue.requested_by_id)}</span>
            </PropRow>
          )}
          {(issue.created_via === "dialog" || issue.created_via === "agent" || issue.created_via === "cli") && (
            <PropRow label={t(($) => $.detail.prop_created_via)}>
              <span className="truncate">
                {issue.created_via === "dialog"
                  ? t(($) => $.detail.created_via_dialog)
                  : issue.created_via === "agent"
                    ? t(($) => $.detail.created_via_agent)
                    : t(($) => $.detail.created_via_cli)}
              </span>
            </PropRow>
          )}

          {addPropertyRow}
        </IssueSidebarPropertyGrid></IssueSidebarCollapsible>
      </div>
    ),
    // Quick actions — the sidebar's only "do something" block, so it sits
    // directly under Properties and above every read-only section. Renders
    // nothing when the workspace has no active action visible to this
    // member. It is NOT filtered by invoke permission: a member can see and
    // click an action they cannot run, and the refusal is explained at run
    // time rather than by a silently shorter list.
    quick_actions: <QuickActionsSection issueId={issue.id} />,
    wakeups: (
      <WakeupsSection
        issueId={issue.id}
        closed={issueClosed}
        defaultAgentId={issue.assignee_type === "agent" ? (issue.assignee_id ?? undefined) : undefined}
        projectId={issue.project_id ?? undefined}
        reveal={sidebarReveal === "wakeups"}
        onRevealEnd={endSidebarReveal}
      />
    ),
    plugins: <PluginPanelSection issueId={issue.id} />,
    // The fork shows the parent in the hierarchy minimap (`IssueHierarchy`),
    // which replaces the standalone Parent issue section. Guests get no
    // minimap, so they keep the upstream section.
    parent: guest && parentIssue ? (
        <div>
          <IssueSidebarSectionHeader title={t(($) => $.detail.section_parent_issue)} open={parentIssueOpen} onOpenChange={setParentIssueOpen}
            actions={<IconButton size="icon-xs" label={t(($) => $.actions.remove_parent_issue)} onClick={() => actions.removeParent()}><Unlink /></IconButton>} />
          <IssueSidebarCollapsible open={parentIssueOpen}><div className="px-2">
            <div className="flex items-center gap-0.5 rounded-md px-2 hover:bg-accent/50 transition-colors group">
              <AppLink
                href={paths.issueDetail(issueRouteRef(parentIssue))}
                className="flex flex-1 min-w-0 items-center gap-1.5 py-2 text-label"
              >
                <StatusIcon
                  status={parentIssue.status}
                  color={resolveStatusColor(parentIssue.status)}
                  icon={resolveStatusIcon(parentIssue.status)}
                  category={issueStatusCategory(parentIssue) ?? undefined}
                  className="h-3.5 w-3.5 shrink-0"
                />
                <span className="text-muted-foreground shrink-0">{parentIssue.identifier}</span>
                <span className="truncate group-hover:text-foreground">{parentIssue.title}</span>
              </AppLink>

            </div>
          </div></IssueSidebarCollapsible>
        </div>
    ) : null,
    // Blockers, the issues this one blocks, and duplicates (MUL-10050).
    relations: <IssueRelationsSection issue={issue} />,
    // Pull requests — unavailable when the workspace disables the PR sidebar
    // (or the GitHub master switch is off). Backend data is kept either
    // way so re-enabling restores the section instantly.
    pull_requests: (
      <PullRequestsSection
        issueId={id}
        identifier={issue.identifier}
        open={pullRequestsOpen}
        onOpenChange={setPullRequestsOpen}
        reveal={sidebarReveal === "pull_requests"}
        onRevealEnd={endSidebarReveal}
      />
    ),
    // Environments the issue's runs left running, and the "+" that starts one.
    environments: (
      <IssueEnvironmentsSection
        issueId={id}
        assigneeType={issue.assignee_type}
        assigneeId={issue.assignee_id}
        reveal={sidebarReveal === "environments"}
        onRevealEnd={endSidebarReveal}
      />
    ),
    // Deliverables — the files this issue's comments delivered. Hidden
    // while there are none.
    deliverables: (
      <DeliverablesSection
        files={deliverableFiles}
        onOpenOverview={openOverview}
        onCopyLink={copyDeliverableLink}
      />
    ),
    // Apps the issue's runs used. Hidden until one makes a call.
    apps: <IssueAppsSection issueId={id} />,
    // Execution log — active runs + collapsed past runs, each carrying its
    // own token spend, with the issue total on the section header.
    // Self-contained; owns its own collapse state and WS subscriptions.
    // Hides itself when there are no runs to show.
    execution_log: <ExecutionLogSection key={id} issueId={id} identifier={issue.identifier} issueTitle={issue.title} hasChildren={childIssues.length > 0} />,
    metadata: <MetadataSection metadata={issue.metadata ?? {}} />,
  };

  const sidebarContent = landingChromeHeld ? null : (
    <div className="space-y-5">
      {/* Hierarchy minimap — parent, siblings and children at a glance; replaces
          the Parent issue section and the Relations graph button. Guests keep
          the plain Parent section: the minimap reads siblings they cannot see. */}
      {!guest && <IssueHierarchy key={issue.id} issue={issue} />}

      {!guest && <RunBudgetMeter key={issue.id} issueId={issue.id} />}

      <IssueSidebarSections
        sections={sidebarSections}
        available={ISSUE_SIDEBAR_SECTIONS.filter(
          (section) =>
            guest
              ? GUEST_SIDEBAR_SECTIONS.has(section)
              : section !== "parent" &&
                (section !== "pull_requests" || githubSettings.prSidebar) &&
                (section !== "plugins" || pluginsEnabled),
        )}
        addRow={<IssueSidebarAddRow items={sidebarAddables.filter((item) => item !== sidebarReveal)} onPick={setSidebarReveal} issue={issue} />}
        footer={guest ? undefined : <IssueSidebarFooter issue={issue} />}
      />

      {!guest && <StatusHistoryButton issueId={id} wsId={wsId} projectId={issue.project_id} workflowId={issue.workflow_id} />}
    </div>
  );

  // Shared localized explanation for every disclosure toggle find disables
  // (comment thread Collapse chevron here, the description Show-less button
  // below) — 01-DESIGN "disabled with a localized explanation".
  const findCollapseDisabledReason = t(($) => $.detail.find.collapse_disabled);
  // Row renderer for the virtualized timeline.
  // The wrapper `id="comment-..."` is the deep-link target — equivalent to
  // a native `<a href="#comment-...">` anchor. A run that left no comment is
  // addressed by its task id, which the sidebar's unread link lands on.
  const renderItem = (_i: number, item: TimelineItem): React.ReactElement => {
    if (item.kind === "run") {
      const reply = item.entry;
      return <div className={!item.row || item.row.last ? "pb-3" : undefined} id={`comment-${item.id}`}>
        {reply?.resolved_at && !effectiveExpandedResolved.has(reply.id) ? <ResolvedThreadBar
          entry={reply} replies={timelineView.threadReplies.get(reply.id) ?? EMPTY_REPLIES}
          onExpand={() => toggleResolvedExpand(reply.id, true)} /> : <AgentRunComment run={item.run} entering={enteringRunIds.has(item.run.task.id)} standalone
          highlightedId={highlightedId}
          commentProps={reply ? {
            virtualRow: item.row,
            onRevealAnnotation: revealAnnotation,
            pendingAnnotationId: pendingAnnotationEdit?.rootId === reply.id && pendingAnnotationEdit.sourceCommentId === (item.row?.id ?? reply.id)
              ? pendingAnnotationEdit.annotationId : undefined,
            onAnnotationEditComplete: completeAnnotationEdit,
            issueId: id, replyScrollParent: scrollContainerEl, entry: reply, replies: timelineView.threadReplies.get(reply.id) ?? EMPTY_REPLIES,
            currentUserId: user?.id, canModerate: canModerateComments, onReply: submitReply,
            onReplyAccepted: scrollToTimelineBottom, onEdit: editComment, onDelete: deleteComment,
            onToggleReaction: handleToggleReaction, onCreateSubIssue: guest ? undefined : openCommentSubIssue,
            onResolveToggle: handleResolveToggle,
            onCopyLink: actions.copyCommentLink, onJumpToComment: jumpToComment,
            onCollapseResolved: reply.resolved_at ? () => toggleResolvedExpand(reply.id, false) : undefined,
            expandedResolvedIds: effectiveExpandedResolved, onResolvedExpandChange: toggleResolvedExpand,
            highlightedCommentId: highlightedId,
            runs: commentRuns.get(reply.id) ?? EMPTY_COMMENT_RUNS,
            enteringRunIds: enteringRunIdsByThread.get(reply.id),
            threadLengthExpanded: expandedThreadLengths.has(reply.id),
            onThreadLengthExpandChange: toggleThreadLengthExpand,
            forceThreadExpanded: forceThreadOpen(reply.id),
            forceThreadLengthExpanded: forceThreadLengthExpand(reply.id),
            onManualCollapseChange: handleManualCollapseChange,
            findManualCollapseDisabled: findRootId === reply?.id,
            findManualCollapseDisabledReason: findCollapseDisabledReason,
          } : undefined} />}
      </div>;
    }
    if (item.kind === "resolved-bar") {
      return (
        <ResolvedToggleReveal id={item.id} toggledRef={recentResolvedToggleRef} className="pb-3" domId={`comment-${item.id}`}>
          <ResolvedThreadBar
            entry={item.entry}
            replies={timelineView.threadReplies.get(item.id) ?? EMPTY_REPLIES}
            onExpand={() => toggleResolvedExpand(item.id, true)}
          />
        </ResolvedToggleReveal>
      );
    }
    if (item.kind === "comment" || item.kind === "thread-row") {
      const rootId = item.entry.id;
      const isResolved = !!item.entry.resolved_at;
      return (
        <ResolvedToggleReveal
          id={item.id}
          toggledRef={recentResolvedToggleRef}
          className={item.row?.last ? "pb-3" : undefined}
          domId={item.kind === "comment" ? `comment-${item.id}` : undefined}
        >
          <CommentCard
            virtualRow={item.row}
            onRevealAnnotation={revealAnnotation}
            pendingAnnotationId={pendingAnnotationEdit?.rootId === rootId && pendingAnnotationEdit.sourceCommentId === (item.row?.id ?? item.id)
              ? pendingAnnotationEdit.annotationId : undefined}
            onAnnotationEditComplete={completeAnnotationEdit}
            issueId={id}
            replyScrollParent={scrollContainerEl}
            runs={commentRuns.get(rootId) ?? EMPTY_COMMENT_RUNS}
            enteringRunIds={enteringRunIdsByThread.get(rootId)}
            entry={item.entry}
            replies={timelineView.threadReplies.get(rootId) ?? EMPTY_REPLIES}
            currentUserId={user?.id}
            canModerate={canModerateComments}
            onReply={submitReply}
            onReplyAccepted={scrollToTimelineBottom}
            onEdit={editComment}
            onDelete={deleteComment}
            onToggleReaction={handleToggleReaction}
            onCreateSubIssue={guest ? undefined : openCommentSubIssue}
            onResolveToggle={handleResolveToggle}
            onCopyLink={actions.copyCommentLink}
            onJumpToComment={jumpToComment}
            onCollapseResolved={isResolved ? () => toggleResolvedExpand(rootId, false) : undefined}
            expandedResolvedIds={effectiveExpandedResolved}
            onResolvedExpandChange={toggleResolvedExpand}
            highlightedCommentId={highlightedId}
            threadLengthExpanded={expandedThreadLengths.has(rootId)}
            onThreadLengthExpandChange={toggleThreadLengthExpand}
            forceThreadExpanded={forceThreadOpen(rootId)}
            forceThreadLengthExpanded={forceThreadLengthExpand(rootId)}
            onManualCollapseChange={handleManualCollapseChange}
            findManualCollapseDisabled={findRootId === rootId}
            findManualCollapseDisabledReason={findCollapseDisabledReason}
          />
        </ResolvedToggleReveal>
      );
    }
    // activity-group
    const expanded = expandedActivityIds.has(item.id)
      ? true
      : collapsedActivityIds.has(item.id)
        ? false
        : item.id === lastActivityGroupId || handoffActivityGroupIds.has(item.id);
    const truncateOlder = item.id === lastActivityGroupId;
    const showOlder = showOlderActivityIds.has(item.id);
    return (
      <ActivityBlock
        issueId={id}
        entries={item.entries}
        expanded={expanded}
        onToggle={() => toggleActivityBlock(item.id, expanded)}
        truncateOlder={truncateOlder}
        showOlder={showOlder}
        onToggleShowOlder={() => showOlderActivities(item.id)}
        getActorName={getActorName}
        resolveStatusLabel={resolveStatusLabel}
        resolveStatusCategory={resolveStatusCategory}
        resolveStatusColor={resolveStatusColor}
        resolveStatusIcon={resolveStatusIcon}
        t={t}
        timeAgo={timeAgo}
        locale={locale}
        requesterName={requesterName}
      />
    );
  };

  // Breadcrumb shows the single most-direct container, never a fabricated chain.
  // project_id and parent_issue_id are orthogonal (a sub-issue can live in a
  // different project than its parent), so we never render both: parent wins,
  // else project, else nothing. The project is still shown in the properties
  // panel. The workspace name is intentionally absent — "all issues" is a view,
  // not a container. On the Inbox page the issue is read as a notification:
  // the list it came from is beside it (or named by a phone's back control),
  // so the header holds the issue alone.
  const breadcrumbSegments: BreadcrumbSegment[] = inbox
    ? []
    : parentIssue
      ? [{ href: paths.issueDetail(issueRouteRef(parentIssue)), label: parentIssue.identifier }]
      : breadcrumbProject
        ? [
            {
              href: paths.projectDetail(breadcrumbProject.id),
              className: "flex items-center gap-1 min-w-0 max-w-72",
              label: (
                <>
                  <ProjectIcon project={breadcrumbProject} size="sm" />
                  <span className="min-w-0 truncate">{breadcrumbProject.title}</span>
                </>
              ),
            },
          ]
        : [];

  const titleSizeClass = isPeek ? "text-title-lg font-semibold" : ISSUE_PAGE_TITLE_CLASS;

  // What the header names the issue: the identifier only in a peek, where the
  // title is right below it; identifier and title on the page.
  const headerLeafLabel = isPeek ? issue.identifier : `${issue.identifier} ${issue.title}`;
  const inboxIssueActions = guest ? undefined : (
    <InboxIssueActions
      issue={issue}
      onUpdate={handleUpdateField}
      onMarkDuplicate={actions.openMarkDuplicate}
      isDuplicate={isDuplicateIssue(issue)}
    />
  );
  const inboxBar = inbox?.bar(inboxIssueActions);
  const jumpToLatestButton = jumpToLatest.visible ? (
    <JumpToLatestButton
      key="jump-to-latest"
      onClick={onJumpToLatest}
      label={jumpToLatestLabel}
      count={newComments.ids.length}
    />
  ) : null;

  const showHeaderIssueActions = !guest && !inbox && !isPeek && !isMobile && !sidebarOpen;

  const detailContent = (
    <CurrentIssueRenderContextProvider value={currentIssueRenderContext}>
    <div
      data-mc="issue-detail"
      data-status={issue.status}
      data-priority={issue.priority}
      data-view={isPeek ? "peek" : "page"}
      className="relative flex h-full min-w-0 flex-1 flex-col"
    >
        {/* In-page find bar — floats over the top-right of the content column
            (below the breadcrumb header), outside the scroll container so it
            stays put while the timeline scrolls and its own text isn't walked.
            z-30: must beat every sticky affordance pinned at the timeline's
            top-0 (comment headers z-10, resolve collapse bars z-20) — at equal
            z the later-in-DOM sticky bar paints over the find bar and orphans
            its close button (MUL-4414). On desktop it also steps inside the
            thread rail's right-edge strip, so an open find bar can't cover
            the topmost ticks. */}
        <AnimatePresence initial={false}>
          {find.open && (
            <FindBar
              key="find-bar"
              find={find}
              className={cn("absolute top-14 z-30", isMobile ? "right-4" : "right-10")}
            />
          )}
        </AnimatePresence>
        <BreadcrumbHeader
          className="@container/issue-header"
          leading={leadingAction}
          segments={breadcrumbSegments}
          leaf={
            // The header shares its line with the actions, so a long title is
            // cut off; hover (or focus) reveals it for the whole header width.
            // Below the trigger, since the header sits at the top of the page.
            <TruncatedTooltip content={headerLeafLabel} side="bottom">
              <AppLink
                href={paths.issueDetail(issueRouteRef(issue))}
                className="flex min-w-0 transition-opacity hover:opacity-80"
              >
                <span className="truncate font-medium text-foreground">
                  {headerLeafLabel}
                </span>
              </AppLink>
            </TruncatedTooltip>
          }
          actions={
            <>
            {/* What is happening on the issue right now, left of the actions:
                who else has it open, then which agent is working. Each chip
                hides itself when there is nothing to say; the hairline before
                the actions shows only while one of them does. */}
            {!guest && (
              <div className="flex items-center gap-1 empty:hidden after:ml-1 after:h-4 after:w-px after:bg-border">
                <IssueWakeupHeaderChip issueId={id} onOpen={openWakeups} />
                {descLive && (
                  <IssuePresenceHeaderChip
                    collaborators={descPresence}
                    locate={descLive.locate}
                    watchCarets={descLive.watchCarets}
                    onJump={jumpToCollaborator}
                  />
                )}
                <IssueAgentHeaderChip issueId={id} onJumpToRun={jumpToRun} handoffRound={handoffRound} />
              </div>
            )}
            {/* With the properties sidebar closed, its most used controls
                move into the header so status, assignee and quick actions
                stay one click away. The Inbox's floating group already
                carries them, and the peek shows them as pills under the
                title. */}
            {showHeaderIssueActions && (
              <div
                data-testid="issue-header-actions"
                className="flex min-w-0 items-center gap-0.5 after:ml-1 after:h-4 after:w-px after:bg-border"
              >
                <IssueBarActions
                  issue={issue}
                  onUpdate={handleUpdateField}
                  onMarkDuplicate={actions.openMarkDuplicate}
                  isDuplicate={isDuplicateIssue(issue)}
                  container="issue-header"
                  side="bottom"
                />
              </div>
            )}
            {inbox && (
              <IconButton
                label={t(($) => $.detail.open_issue_page)}
                tooltipSide="bottom"
                className="text-muted-foreground"
                nativeButton={false}
                render={<AppLink href={paths.issueDetail(issueRouteRef(issue))} />}
              >
                <ArrowUpRight />
              </IconButton>
            )}
            {!guest && <IssueDetailActionsDropdown
              issue={issue}
              locateInSidebar={!isPeek}
              align="end"
              // When a parent passes `onDelete`, we detect deletion via effect
              // above and skip navigation. Otherwise the modal takes us back
              // to the list we came from, falling back to all issues.
              onDeletedFallbackPath={onDelete ? undefined : paths.issues()}
              archivedInboxItemId={archivedInboxItemId}
              rowMenu={pageMenu.rowMenu}
              trigger={
                <IconButton
                  label={tCommon(($) => $.more_actions)}
                  tooltipSide="bottom"
                  className="text-muted-foreground"
                >
                  <MoreHorizontal />
                </IconButton>
              }
            />}
            {isPeek ? (
              trailingActions
            ) : (
              <IconButton
                label={t(($) => $.detail.sidebar_tooltip)}
                shortcut={<ActionShortcutKeycaps action="toggleRightSidebar" />}
                tooltipSide="bottom"
                variant={sidebarOpen ? "secondary" : "ghost"}
                className={sidebarOpen ? "" : "text-muted-foreground"}
                onClick={handleToggleSidebar}
              >
                <PanelRight />
              </IconButton>
            )}
            </>
          }
        />

        {/* scrollbar-gutter both-edges: with classic (space-taking) scrollbars —
            macOS with a mouse or "always show", Windows, Linux — the global
            `scrollbar-width: thin` carves ~11px off the right side only, so the
            centered column reads 32px left vs 43px right (MUL-4404). Mirroring
            the gutter restores symmetry; overlay-scrollbar platforms reserve
            nothing and render unchanged. */}
        <RichContentScrollRootProvider scrollRoot={scrollContainerEl}>
        <div
          ref={attachScrollContainer}
          data-tab-scroll-root={isPeek ? undefined : scrollContainerKey}
          data-issue-timeline-scroll
          // `tabIndex={-1}`: programmatic-focus-only. Closing find moves
          // focus here (01-DESIGN "move focus from the closing bar to a
          // stable issue control") — without a tab index, `.focus()` on a
          // plain <div> is a no-op in every real browser, silently stranding
          // focus on the just-unmounted find bar's DOM node.
          tabIndex={-1}
          className="@container relative flex-1 overflow-y-auto [scrollbar-gutter:stable_both-edges] focus:outline-none"
          onContextMenu={pageMenu.onContextMenu}
        >
        {/* Gutters: 32px is a comfortable reading margin on a desktop column
            but eats 16% of a 393px phone, so below `md` they drop to 12px.
            `max-md:pb-chat-launcher` reserves the launcher's corner at the end
            of the scroll: below `md` the composer is not pinned (see
            `useStickyComposer`), so it lands here — right where the launcher
            floats — once the reader scrolls to the bottom. */}
        <div
          className={cn(
            // The peek is already a narrow column, so it takes a tighter gutter
            // and never reaches the chat launcher's corner (see IssuePeekHost).
            isPeek
              ? "mx-auto w-full px-6 py-5"
              : cn(READING_COLUMN, "px-[var(--space-sm,0.75rem)] py-[var(--space-lg,1.5rem)] max-md:pb-chat-launcher md:px-[var(--space-xl,2rem)] md:py-[var(--space-xl,2rem)]"),
            !reduceMotion &&
              activeInboxTransition &&
              (activeInboxTransition === "next"
                ? "animate-inbox-enter-next"
                : "animate-inbox-enter-prev"),
          )}
        >
          <IssueDuplicateBanner
            issue={issue}
            onUnmark={() =>
              handleUpdateField(
                { status: "todo" },
                { onSuccess: () => toast.success(t(($) => $.duplicates.unmark_toast)) },
              )
            }
          />
          <div data-mc="issue-title">
          {guest && <div id={`find-title-${id}`} className={cn("w-full leading-snug", titleSizeClass)}>{issue.title}</div>}
          {!guest && titleLazy.active && (
            <div id={`find-title-${id}`} className={titleLazy.ready ? undefined : "hidden"}>
              <TitleEditor
                key={`title-${id}-${titleResetToken}`}
                ref={titleEditorRef}
                defaultValue={titleConflictDraft ?? titleDraft.value ?? issue.title}
                placeholder={t(($) => $.detail.title_placeholder)}
                className={cn("w-full leading-snug", titleSizeClass)}
                onReady={titleLazy.onReady}
                onChange={(value) => {
                  if (value.trim() === issue.title && titleSavesInFlightRef.current === 0) clearTitleDraft();
                  else setTitleDraft({ value, base: titleBaseRef.current ?? issue.title });
                }}
                onBlur={(value) => {
                  const trimmed = value.trim();
                  if ((!trimmed || trimmed === issue.title) && titleSavesInFlightRef.current === 0) clearTitleDraft();
                  if (trimmed && trimmed !== issue.title) {
                    titleSavesInFlightRef.current += 1;
                    handleUpdateField({
                      title: trimmed,
                      title_base: titleBaseRef.current,
                    }, {
                      onSuccess: (serverIssue) => {
                        titleSavesInFlightRef.current = Math.max(0, titleSavesInFlightRef.current - 1);
                        setTitleConflictDraft(null);
                        // Edits made while this save was in flight are still unsent, even one that
                        // went back to the old title. Only text the server now holds is done.
                        const latest = useIssueTitleDraftStore.getState().getDraft(id)?.value?.trim();
                        if (latest == null || latest === trimmed || latest === serverIssue.title) clearTitleDraft();
                        titleBaseRef.current = serverIssue.title;
                      },
                      onError: (error) => {
                        titleSavesInFlightRef.current = Math.max(0, titleSavesInFlightRef.current - 1);
                        if (errorCode(error) === "revision_conflict") {
                          setTitleConflictDraft(trimmed);
                        }
                      },
                    });
                  }
                }}
              />
            </div>
          )}
          {!guest && !titleLazy.ready && (
            <div
              role="button"
              tabIndex={0}
              className={cn("w-full cursor-text leading-snug", titleSizeClass)}
              onClick={(e) => {
                // A drag-selection (copying the title) must not summon the editor.
                const sel = window.getSelection();
                if (sel && !sel.isCollapsed) return;
                titleBaseRef.current = issue.title;
                titleLazy.activate({ x: e.clientX, y: e.clientY });
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  titleBaseRef.current = issue.title;
                  titleLazy.activate();
                }
              }}
            >
              <span id={titleLazy.active ? undefined : `find-title-${id}`}>{issue.title}</span>
            </div>
          )}
          </div>
          {titleConflictDraft !== null ? (
            <RevisionConflictCompare
              className="mt-2"
              title={t(($) => $.revision.compare_title)}
              serverLabel={t(($) => $.revision.server_version)}
              localLabel={t(($) => $.revision.local_version)}
              serverValue={issue.title}
              localValue={titleConflictDraft}
              serverAction={(
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    // Local-only: the server already holds this title, so
                    // discarding writes nothing. The remount is what puts it
                    // back into the editor (see titleResetToken).
                    setTitleConflictDraft(null);
                    clearTitleDraft();
                    titleBaseRef.current = issue.title;
                    setTitleResetToken((token) => token + 1);
                  }}
                >
                  {t(($) => $.revision.use_server)}
                </Button>
              )}
              localAction={(
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    const draft = titleConflictDraft.trim();
                    if (!draft) return;
                    handleUpdateField(
                      { title: draft, title_base: issue.title },
                      {
                        onSuccess: (serverIssue) => {
                          setTitleConflictDraft(null);
                          clearTitleDraft();
                          titleBaseRef.current = serverIssue.title;
                        },
                      },
                    );
                  }}
                >
                  {t(($) => $.revision.keep_local)}
                </Button>
              )}
            />
          ) : null}

          {parentIssue && !issue.source_context && (
            <AppLink
              href={paths.issueDetail(issueRouteRef(parentIssue))}
              className="mt-2 inline-flex max-w-full items-center gap-1.5 text-label text-muted-foreground hover:text-foreground transition-colors group/parent"
            >
              <span className="shrink-0">{t(($) => $.detail.sub_issue_of)}</span>
              <StatusIcon
                  status={parentIssue.status}
                  color={resolveStatusColor(parentIssue.status)}
                  icon={resolveStatusIcon(parentIssue.status)}
                  category={issueStatusCategory(parentIssue) ?? undefined}
                  className="h-3.5 w-3.5 shrink-0"
                />
              <span className="tabular-nums shrink-0">{parentIssue.identifier}</span>
              <span className="truncate group-hover/parent:text-foreground">
                {parentIssue.title}
              </span>
              {parentChildIssues.length > 0 && (() => {
                const done = parentChildIssues.filter((c) => issueBehavesAs(c, "done")).length;
                return (
                  <span className="ml-1 inline-flex items-center gap-1 rounded-full bg-muted/60 px-1.5 py-0.5 shrink-0">
                    <ProgressRing done={done} total={parentChildIssues.length} size={11} />
                    <span className="tabular-nums text-micro font-medium">
                      {done}/{parentChildIssues.length}
                    </span>
                  </span>
                );
              })()}
            </AppLink>
          )}

          {isPeek && (
            <div className="mt-3">
              <IssuePropertyPills
                issue={issue}
                onUpdate={handleUpdateField}
                onMarkDuplicate={actions.openMarkDuplicate}
                isDuplicate={isDuplicateIssue(issue)}
              />
            </div>
          )}

          {issue.source_context && (
            <SourceContextBadge
              context={issue.source_context}
              parentIssue={parentIssue}
              parentProgress={parentChildIssues.length > 0
                ? {
                    done: parentChildIssues.filter((child) => issueBehavesAs(child, "done")).length,
                    total: parentChildIssues.length,
                  }
                : undefined}
              refetchIssue={async () => {
                const result = await refetchIssue();
                return result.isError ? null : result.data;
              }}
            />
          )}

          <StatusSummary summary={statusSummaryOf(issue.metadata)} />

          <div
            id={`find-description-${id}`}
            {...(guest ? {} : descDropZoneProps)}
            {...descriptionAnnotations.captureProps}
            ref={descriptionAnnotations.cardRef}
            className="relative mt-5 rounded-lg"
            onFocusCapture={(event) => {
              // Only focus landing IN the editor counts as "editing" for the
              // collapse-disable guard — not the Show more/less button
              // itself. Without this, pressing Enter on "Show more" expands
              // the description, its own button becomes "Show less" on the
              // next render, `descriptionFocused` flips true from this same
              // focus event, and `collapseDisabled` immediately disables the
              // button the user's focus is still on — dropping focus.
              if (!event.target.closest("[data-description-editor]")) return;
              if (!descriptionEditingRef.current) {
                descriptionEditingRef.current = true;
                setDescriptionFocused(true);
              }
              if (!showLiveDesc) setDescSoloHeld(true);
            }}
            onBlurCapture={(event) => {
              if (!event.currentTarget.contains(event.relatedTarget)) {
                descriptionEditingRef.current = false;
                setDescriptionFocused(false);
                setDescSoloHeld(false);
              }
            }}
          >
            {descriptionAnnotations.popup}
            <DescriptionDisclosure
              // Find forces the description open for the duration of the
              // search (`effectiveDescriptionExpanded`); disabling collapse
              // while find.open keeps the reader from hiding a match out from
              // under the search — 01-DESIGN "disclosure toggles that would
              // hide a match are disabled with a localized explanation".
              collapseDisabled={find.open || descriptionFocused || descUploading || descPendingAttachments.length > 0}
              collapseDisabledReason={find.open ? findCollapseDisabledReason : undefined}
              contentVersion={issue.revision}
              expanded={effectiveDescriptionExpanded}
              id={id}
              labels={{
                loading: t(($) => $.detail.description_preview_loading),
                moreLines: (count) => t(($) => $.detail.description_more_lines, { count }),
                preview: t(($) => $.detail.description_preview),
                showLess: t(($) => $.detail.description_show_less),
                showMore: t(($) => $.detail.description_show_more),
              }}
              // While find is open the toggle must not write the base
              // preference (row 58: "no write to any fold preference") — the
              // button itself is disabled above, but this guards direct
              // invocation defensively (e.g. a future caller of onExpandedChange).
              onExpandedChange={(expanded) => {
                if (find.open) return;
                setDescriptionExpanded(id, expanded);
              }}
            >
              <div ref={descContentRef} data-comment-content={descriptionSourceId}>
                {guest ? (
                  <ReadonlyContent content={issue.description ?? ""} attachments={descEditorAttachments} />
                ) : (
                <>
                {descLive?.ready && (
                  <div hidden={!showLiveDesc}>
                    <ContentEditor
                      ref={showLiveDesc ? descEditorRef : undefined}
                      key={`${id}:${descLive.generation}`}
                      collaboration={descLive.collaboration}
                      onReady={() => setDescLiveReadyFor(descLive.generation)}
                      placeholder={t(($) => $.detail.desc_placeholder)}
                      onUploadFile={handleDescriptionUpload}
                      onUploadingChange={onDescUploadingChange}
                      currentIssueId={id}
                      selectionAction={descriptionSelectionAction}
                      attachments={descEditorAttachments}
                    />
                  </div>
                )}
                {!showLiveDesc && (
                <ContentEditor
                  ref={descEditorRef}
                  key={id}
                  value={issue.description ?? ""}
                  persistedMediaMarkdown={issue.description ?? ""}
                  placeholder={t(($) => $.detail.desc_placeholder)}
                  onUpdate={(md, baseMarkdown) => {
                    // Bind any pending uploads still referenced in the markdown
                    // so they appear in `issueAttachments` after refresh and the
                    // editor's text/code preview keeps working past reload.
                    //
                    // Match with `contentReferencesAttachment`, NOT `md.includes(a.url)`:
                    // the editor persists the durable `markdownLink`
                    // (`/api/attachments/<id>/download` / `markdown_url`) into the
                    // body, never the raw storage `a.url`. A bare `md.includes(a.url)`
                    // therefore never matches, so the upload is never linked via
                    // `attachment_ids`. After reload it's absent from
                    // `issueAttachments`, the renderer can't resolve it to a
                    // freshly-signed `download_url`, and the persisted auth-gated
                    // download endpoint fails to load as a native <img> on clients
                    // whose origin isn't the API host (Desktop/Electron, mobile
                    // webview) — while still working on web via the cookie/proxy.
                    // This mirrors the comment/reply/chat composers, which already
                    // bind via `contentReferencesAttachment` (MUL-3130 / MUL-3192).
                    const ids = descPendingAttachmentsRef.current
                      .filter((a) => contentReferencesAttachment(md, a))
                      .map((a) => a.id);
                    queueDescriptionSave({
                      markdown: md,
                      baseMarkdown,
                      attachmentIds: ids,
                    });
                  }}
                  onUploadFile={handleDescriptionUpload}
                  onUploadingChange={onDescUploadingChange}
                  debounceMs={1500}
                  // Closing the issue modal must save what the user last saw —
                  // without the flush, a paste followed by a quick close loses
                  // the image markdown and its attachment_ids bind (MUL-3254).
                  flushPendingOnUnmount
                  currentIssueId={id}
                  selectionAction={descriptionSelectionAction}
                  attachments={descEditorAttachments}
                />
                )}
                </>
                )}
              </div>
            </DescriptionDisclosure>

            <AttachmentList
              attachments={chainedAttachments}
              content={issue?.description ?? ""}
              className="mt-3"
            />

            {descStorageRefusal && (
              <StorageLimitNotice
                refusal={descStorageRefusal}
                onDismiss={dismissDescStorageRefusal}
                className="mt-2"
              />
            )}

            <div className="flex items-center gap-1 mt-3">
              <ReactionBar
                reactions={issueReactions}
                currentUserId={user?.id}
                onToggle={handleToggleIssueReaction}
                getActorName={getActorName}
              />
              {!guest && (
                <FileUploadButton
                  size="sm"
                  multiple
                  onSelect={uploadIntoExpandedDescription}
                />
              )}
            </div>
            {descDragOver && <FileDropOverlay />}
          </div>

          {/* Sub-issues — Linear-style */}
          {childIssues.length === 0 && !guest && (
            <div className="mt-6">
              <button
                type="button"
                className="inline-flex items-center gap-1.5 text-label text-muted-foreground hover:text-foreground transition-colors"
                onClick={() => actions.openCreateSubIssue()}
              >
                <Plus className="h-3.5 w-3.5" />
                <span>{t(($) => $.detail.add_sub_issues)}</span>
              </button>
            </div>
          )}
          {childIssues.length > 0 && (() => {
            const doneCount = childIssues.filter((c) => issueBehavesAs(c, "done")).length;
            return (
              // Provider hosts the shared right-click actions menu the rows
              // delegate to (one singleton menu, not one per row).
              <IssueContextMenuProvider>
              <IssueSurfaceSelectionProvider selection={subIssueSelection}>
              <div className="mt-10 group/sub-issues">
                {/* Header */}
                <div className="flex items-center gap-2 mb-2">
                  <button
                    type="button"
                    onClick={() => setSubIssuesCollapsed((v) => !v)}
                    className="flex items-center gap-1.5 text-body font-medium text-foreground hover:text-foreground/80 transition-colors"
                  >
                    <ChevronDown
                      className={cn(
                        "h-3.5 w-3.5 text-muted-foreground transition-transform duration-150 ease-[cubic-bezier(0.23,1,0.32,1)] motion-reduce:transition-none",
                        subIssuesCollapsed && "-rotate-90",
                      )}
                    />
                    <span>{t(($) => $.detail.sub_issues_label)}</span>
                  </button>
                  <div className="inline-flex items-center gap-1.5 rounded-full bg-muted/60 px-2 py-0.5">
                    <ProgressRing done={doneCount} total={childIssues.length} size={11} />
                    <span className="text-micro text-muted-foreground tabular-nums font-medium">
                      {doneCount}/{childIssues.length}
                    </span>
                  </div>
                  {/* issue.id, not the route param — the endpoint takes a
                      UUID and the route may carry a human-readable id. */}
                  {!guest && <SubIssuesAgentWorkingChip parentIssueId={issue.id} />}
                  {!guest && <input
                    type="checkbox"
                    checked={allChildrenSelected}
                    ref={(el) => {
                      if (el) el.indeterminate = someChildrenSelected && !allChildrenSelected;
                    }}
                    onChange={handleToggleSelectAllChildren}
                    aria-label={t(($) => $.detail.select_all_sub_issues_aria)}
                    className={cn(
                      "ml-1 cursor-pointer accent-primary transition-opacity",
                      someChildrenSelected
                        ? "opacity-100"
                        : "opacity-0 group-hover/sub-issues:opacity-100 focus-visible:opacity-100",
                    )}
                  />}
                  {!guest && !issueClosed && (
                    <span className="ml-1 flex min-w-0 items-center gap-0.5 empty:hidden">
                      <SystemWakeupChips workspaceId={wsId} issueId={issue.id} />
                    </span>
                  )}
                  <div className="ml-auto flex items-center gap-0.5">
                    <SubIssueDisplayPopover workspaceProperties={activeWorkspaceProperties} />
                    {!guest && <Tooltip>
                      <TooltipTrigger
                        render={
                          <button
                            type="button"
                            className="inline-flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground transition-colors"
                            onClick={() => actions.openCreateSubIssue()}
                            aria-label={t(($) => $.detail.add_sub_issue_aria)}
                          >
                            <Plus className="h-4 w-4" />
                          </button>
                        }
                      />
                      <TooltipContent side="bottom">{t(($) => $.detail.add_sub_issue_tooltip)}</TooltipContent>
                    </Tooltip>}
                  </div>
                </div>

                {/* Inline batch toolbar — appears next to the rows when
                    selections exist, instead of as a far-away fixed bar. */}
                <BatchActionToolbar
                  issues={childIssues}
                  placement="inline"
                  clearSelectionOnApply
                />

                {/* List */}
                <IssueSidebarCollapsible open={!subIssuesCollapsed}>
                  {(() => {
                    const groups = groupSubIssuesByStage(childIssues);
                    const staged = childIssues.some((c) => c.stage != null);
                    return (
                      <div className="overflow-hidden rounded-lg border bg-card/30 divide-y divide-border/60">
                        {groups.map(({ stage: groupStage, items }) => (
                          <Fragment key={groupStage ?? "unstaged"}>
                            {staged && (
                              <div className="bg-muted/40 px-3 py-1 text-micro font-medium uppercase tracking-wider text-muted-foreground">
                                {groupStage == null
                                  ? t(($) => $.stage.none)
                                  : t(($) => $.stage.value, { n: groupStage })}
                              </div>
                            )}
                            {items.map((child) => (
                              <SubIssueRow
                                key={child.id}
                                child={child}
                                childProgress={subIssueProgress?.get(child.id)}
                                rowProps={subIssueRowProps}
                                customProperties={subIssueCustomProps}
                              />
                            ))}
                          </Fragment>
                        ))}
                      </div>
                    );
                  })()}
                </IssueSidebarCollapsible>
              </div>
              </IssueSurfaceSelectionProvider>
              </IssueContextMenuProvider>
            );
          })()}

          {/* A guest (MUL-9420) is refused the decision endpoints behind this banner. */}
          {!guest && <IssueDecisionBanner issueId={id} status={issue.status} ownerAgentId={issue.assignee_type === "agent" ? issue.assignee_id : null} />}

          <div className="my-8 border-t" />

          {/* Activity / Comments */}
          <div data-mc="issue-activity" data-comment-images={commentImageLayout}>
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-3">
                <h2 className="text-title-sm font-semibold">{t(($) => $.detail.activity_section)}</h2>
              </div>
              <div className="flex items-center gap-2">
                {/* A delegated subscription is one the user never opted into
                    by hand — their agent created this issue for them. Saying
                    so is what keeps it from reading as the product quietly
                    adding them to things (MUL-5483). The server's delivery
                    tier decides it: a project watcher's delegated row still
                    gets every event (MUL-9360). */}
                {isSubscribed && subscriptionDelivery === "delegated" && (
                  <Tooltip>
                    {/* Quiet surface, not plain body text: this is metadata
                        explaining a state, and must not read as a second
                        action sitting next to Unsubscribe. Uses the shared
                        caption role rather than an ad-hoc size (MUL-5451). */}
                    <TooltipTrigger className="cursor-default rounded-full bg-muted px-2 py-0.5 text-caption text-muted-foreground">
                      {t(($) => $.detail.delegated_subscription_badge)}
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-64">
                      {t(($) => $.detail.delegated_subscription_hint)}
                    </TooltipContent>
                  </Tooltip>
                )}
                {isSubscribed && (
                  <KeyUpdatesSubscriptionBadge
                    delivery={subscriptionDelivery}
                    pending={togglePending}
                    onFollowAll={followAllActivity}
                  />
                )}
                {/* Inherited from the project: say so, and that leaving
                    here only covers this issue, so Unsubscribe is not read as
                    leaving the whole project. */}
                {isSubscribed && subscriptionReason === "project" && (
                  <Tooltip>
                    <TooltipTrigger className="cursor-default rounded-full bg-muted px-2 py-0.5 text-caption text-muted-foreground">
                      {t(($) => $.detail.project_subscription_badge)}
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-64">
                      {t(($) => $.detail.project_subscription_hint)}
                    </TooltipContent>
                  </Tooltip>
                )}
                {/* Nothing until the subscribers query resolves: the default
                    empty list reads as "not subscribed" for everyone, so
                    rendering it flashes Subscribe at someone who is already
                    subscribed, and a click landing in that window sends a
                    subscribe instead of the unsubscribe they meant. An
                    unresolved state is better shown as no control than as the
                    wrong one (MUL-5714). */}
                {subscriptionKnown &&
                  (!isSubscribed || (childCountKnown && childIssues.length === 0) ? (
                    /* One button, no menu, when there is nothing for the
                       subtree option to cover. This is the root-only path
                       (opt_out_scope='issue'), NOT the subtree one: even at
                       zero children the two are different server writes,
                       because a subtree tombstone also keeps FUTURE children
                       from re-subscribing the user
                       (server/pkg/db/queries/subscriber.sql). Declining one
                       issue must not silently opt someone out of a tree that
                       does not exist yet. While the child count is unknown we
                       keep the menu below — it never picks a scope for the
                       user. */
                    <button
                      type="button"
                      onClick={handleToggleSubscribe}
                      disabled={togglePending || !user?.id}
                      className={SUBSCRIPTION_ACTION_CLASS}
                    >
                      {isSubscribed
                        ? t(($) => $.detail.unsubscribe)
                        : t(($) => $.detail.subscribe)}
                    </button>
                  ) : (
                    <DropdownMenu>
                      <DropdownMenuTrigger
                        disabled={togglePending || subtreePending || !user?.id}
                        className={SUBSCRIPTION_ACTION_CLASS}
                      >
                        {t(($) => $.detail.unsubscribe)}
                      </DropdownMenuTrigger>
                      {/* onClick, not onSelect: Base UI's Menu.Item exposes no
                          onSelect (that is the Radix spelling), and because its
                          props extend the full div attribute set, an onSelect
                          typechecks and silently lands on the DOM node as the
                          native text-selection event — the handler never runs
                          (MUL-5710). */}
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem
                          onClick={handleToggleSubscribe}
                          disabled={togglePending}
                        >
                          {t(($) => $.detail.unsubscribe_this)}
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          onClick={handleUnsubscribeSubtree}
                          disabled={subtreePending}
                        >
                          {t(($) => $.detail.unsubscribe_subtree)}
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  ))}
                {/* A guest sees who follows the issue but changes only its own
                    subscription; the server refuses the rest. */}
                {guest ? (
                  subscribers.length > 0 && <SubscriberAvatars subscribers={subscribers} />
                ) : (
                  <Popover>
                    <PopoverTrigger className="cursor-pointer hover:opacity-80 transition-opacity">
                      {subscribers.length > 0 ? (
                        <SubscriberAvatars subscribers={subscribers} />
                      ) : (
                        <span className="flex items-center justify-center h-6 w-6 rounded-full border border-dashed border-muted-foreground/30 text-muted-foreground">
                          <Users className="h-3 w-3" />
                        </span>
                      )}
                    </PopoverTrigger>
                    <SubscriberPopoverContent
                      members={members}
                      agents={agents}
                      subscribers={subscribers}
                      toggleSubscriber={toggleSubscriber}
                      togglesDisabled={
                        !subscriptionKnown || togglePending || !user?.id
                      }
                      t={t}
                    />
                  </Popover>
                )}
              </div>
            </div>

            <LocalDirectoryHint
              projectId={issue?.project_id}
              assignedAgent={issue?.assignee_type === "agent"
                ? agents.find((agent) => agent.id === issue.assignee_id) ?? null
                : undefined}
            />

            {/* The "agent is working" live signal now lives in the header
                (IssueAgentHeaderChip) so it stays in one fixed place and
                doesn't compete with sticky banners in this content column.
                The per-task timeline + past runs live in the right panel
                via ExecutionLogSection. */}

            {/* Timeline entries — virtualized via react-virtuoso to keep
                first-paint cost O(viewport) instead of O(N). On a 500-comment
                issue the unvirtualized .map froze the page for several
                seconds (markdown parse + lowlight code highlight runs per
                CommentCard on mount).

                customScrollParent guard: callback ref populates after the
                first commit. Without this null guard Virtuoso falls back to
                its own scroller, grabs 0 height inside overflow-y-auto, and
                miscomputes total-height on first paint. */}
            {timelineLoading && timelineView.groups.length === 0 ? (
              <TimelineSkeleton />
            ) : (
                !scrollContainerEl ? (
                  // Skeleton while the callback ref populates so the gap
                  // between IssueDetail mount and Virtuoso mount doesn't
                  // flash empty.
                  <TimelineSkeleton />
                ) : (
                  <ImageThumbnailsProvider enabled={commentImageLayout === "thumbnails"}>
                  <div className="mt-4">
                    <SupersessionRevisionsProvider entries={timeline}>
                    <Virtuoso
                      key={`${wsId}:${id}`}
                      ref={virtuosoRef}
                      customScrollParent={scrollContainerEl}
                      data={items}
                      {...(scrollAnchor && scrollAnchorIndex >= 0
                        ? { initialTopMostItemIndex: { index: scrollAnchorIndex, align: "start" as const, offset: scrollAnchor.offset } }
                        : openAtLandingIndex >= 0
                          ? { initialTopMostItemIndex: { index: openAtLandingIndex, align: "start" as const, offset: -16 } }
                          : {})}
                      initialScrollTop={scrollAnchorIndex >= 0 || openAtLandingIndex >= 0 ? undefined : restoredScrollTop}
                      defaultItemHeight={TIMELINE_ROW_ESTIMATED_HEIGHT}
                      increaseViewportBy={{ top: 800, bottom: 800 }}
                      computeItemKey={(_i, item) => timelineItemKey(item)}
                      skipAnimationFrameInResizeObserver
                      // followOutput intentionally NOT set. Virtuoso treats
                      // it as a sticky "is at bottom" flag and resets
                      // scrollTop to maxScrollTop on every height-change
                      // tick — issue-detail is document-shaped, not chat.
                      itemContent={renderItem}
                      rangeChanged={onRowRangeChanged}
                    />
                    </SupersessionRevisionsProvider>
                  </div>
                  </ImageThumbnailsProvider>
                )
            )}

          </div>

          {/* Bottom comment input — no avatar, full width. Direct child of
              the content column (not the Activity section): a sticky box
              can't leave its containing block, and the Activity div only
              spans the timeline — at column level `sticky bottom-0` can pin
              the composer across the whole scroll range.

              Sticky treatment, bottom edge: opaque bg-background
              under/around the card (its rounded
              corners would otherwise leak scrolled text), a 16px gradient
              fade above (exactly the mt-4 gap, so at rest it sits over the
              page background and is invisible), and pb-4 so the card floats
              off the viewport edge — with -mb-4 giving the padding back to
              the column's py-8 so the at-rest layout doesn't shift. */}
          {/* The composer dock: the Inbox's group and "Jump to latest"
              (MUL-8324, MUL-8951) float just above the composer's top edge.
              With the sticky composer the dock rides the composer itself;
              without it, it rides a zero-height sticky anchor just above the
              composer, which holds it off the viewport bottom (clear of a
              phone's chat launcher) until the composer scrolls in. Either way
              the Inbox's group gets its own room at the end of the timeline
              (the anchor's or the composer's margin), so it never covers the
              last comment for good. */}
          <IssueAppRequests issueId={id} highlightId={highlightAppRequestId} highlightToken={highlightRequestToken} />
          {!stickyComposer && (
            <div className={cn("sticky bottom-6 z-10 h-0 max-md:above-chat-launcher", inboxBar && "mt-14")}>
              <ComposerDock className="bottom-0" inboxBar={inboxBar} jump={jumpToLatestButton} />
            </div>
          )}
          <div
            ref={composerRef}
            data-comment-images={commentImageLayout}
            className={cn(
              !inboxBar ? "mt-4" : stickyComposer ? "mt-18" : "mt-3",
              stickyComposer &&
                "sticky bottom-0 z-10 -mb-4 bg-background pb-4 before:pointer-events-none before:absolute before:inset-x-0 before:bottom-full before:h-4 before:bg-gradient-to-t before:from-background before:to-transparent",
            )}
          >
            {/* key={id}: web's /issues/[id] route doesn't remount on
                issueId change, so without an explicit key the editor
                keeps the previous issue's in-memory content and the
                next keystroke would flush it into the new issue's
                draft key. */}
            {stickyComposer && (
              <ComposerDock className="bottom-full mb-3" inboxBar={inboxBar} jump={jumpToLatestButton} />
            )}
            {!landingChromeHeld && (
              <CommentInput
                key={id}
                issueId={id}
                onSubmit={submitComment}
                annotationComposer={descriptionAnnotations.composer}
                onAccepted={scrollToTimelineBottom}
                onEditAnnotation={(annotationId) => descriptionAnnotations.editAnnotation(annotationId, true)}
              />
            )}
          </div>
        </div>
        </div>

        </RichContentScrollRootProvider>

        {/* Thread quick-jump rail — rides the scroll container's right edge,
            next to the scrollbar, where the pointer already is while
            scrolling (MUL-4522). right-3 is the inset that works in both
            scrollbar modes: it clears a classic scrollbar's ~11px gutter,
            and the rail's own 20px width lands it exactly on the content
            column's px-8 padding when the gutter is 0 (overlay scrollbars),
            so it covers neither the scrollbar nor body text. It also clears
            the resize handle's 4px drag strip at the panel edge. Hover
            previews a comment, click jumps to it. Hidden on mobile: no
            hover, and the gutter is too tight. */}
        <NextSelfMention scrollEl={scrollContainerEl} flags={selfMentionFlags} onJump={jumpToSelfMention} />
        {!isMobile && !isPeek && !landingChromeHeld && (
          <ThreadMinimap
            threads={minimapThreads}
            scrollContainerEl={scrollContainerEl}
            onJump={jumpToMinimapTarget}
            mountedRows={mountedRows}
            className="absolute bottom-0 right-3 top-12"
          />
        )}
      </div>
    </CurrentIssueRenderContextProvider>
  );

  // Hosts the one viewer this issue's files page through — see
  // PreviewSequenceProvider. Wraps the column and the sidebar, so the
  // description's files, the timeline's and the sidebar's deliverables all
  // open into one sequence. The versions provider lets each comment's file
  // cards mark a re-uploaded file `v2`.
  const withPreview = (layout: ReactNode) => (
    <MediaSourceProvider source={{ type: "issues", id: issue.id }}>
    <PreviewSequenceProvider
      items={previewSequence}
      describeItem={describeDeliverable}
      onOpenOverview={openOverviewFromViewer}
      onOpened={deliverableLink.onViewerOpened}
      onClosed={deliverableLink.onViewerClosed}
    >
      <AttachmentVersionsProvider files={deliverableFiles}>{layout}</AttachmentVersionsProvider>
      {openDeliverable && (
        <OpenDeliverableOnLoad
          name={openDeliverable}
          files={deliverableFiles}
          read={deliverableLinkRead}
          onOpenedInViewer={deliverableLink.onOpenedInViewer}
          onDone={onDeliverableLinkDoneStable}
        />
      )}
      <DeliverablesOverview
        open={overview.open}
        onClose={closeOverview}
        returnKey={overview.returnKey}
        identifier={issue.identifier}
        files={deliverableFiles}
        commentById={commentById}
        onLocate={locateOrigin}
        onCopyLink={copyDeliverableLink}
      />
    </PreviewSequenceProvider>
    </MediaSourceProvider>
  );

  // The peek has no room for the properties sidebar: its core properties are
  // the pills under the title, and the rest is one click away on the page.
  if (isPeek) {
    return withPreview(<div className="flex flex-1 min-h-0">{detailContent}</div>);
  }

  if (isMobile) {
    return withPreview(
      <div className="flex flex-1 min-h-0">
        {detailContent}
        <Sheet open={mobileSidebarOpen} onOpenChange={setMobileSidebarOpen}>
          <SheetContent
            side="right"
            showCloseButton={false}
            className="w-[320px] overflow-y-auto p-4"
            onKeyDown={letSidebarSettingsDragKeysThrough}
          >
            {sidebarContent}
          </SheetContent>
        </Sheet>
      </div>,
    );
  }

  return withPreview(
    <ResizablePanelGroup orientation="horizontal" className="flex-1 min-h-0" defaultLayout={defaultLayout} onLayoutChanged={onLayoutChanged}>
      <ResizablePanel id="content" minSize="50%">
        {detailContent}
      </ResizablePanel>
      <ResizableHandle />
      <ResizablePanel
        id="sidebar"
        {...rightSidebarPanelMotionProps}
        data-right-sidebar-motion={desktopSidebarMotionEnabled ? "enabled" : undefined}
        defaultSize={desktopSidebarOpen ? 320 : 0}
        minSize={260}
        maxSize={420}
        collapsible
        groupResizeBehavior="preserve-pixel-size"
        panelRef={sidebarRef}
        onResize={handleDesktopSidebarResize}
      >
        {/* Sections inset their own bodies and rows, so the panel itself only
            needs a narrow gutter. pt-2.5 centres the first 28px section
            header on the 48px page header beside it. */}
        <AnimatedRightSidebar open={desktopSidebarVisualOpen} motionEnabled={desktopSidebarMotionEnabled} contentClassName="px-2 pt-2.5 pb-4">
          {sidebarContent}
        </AnimatedRightSidebar>
      </ResizablePanel>
    </ResizablePanelGroup>,
  );
}

/**
 * The ⋯ menu with "Show in sidebar". `useShowInSidebar` reads the shell and
 * sidebar contexts, which change on every nav toggle; reading them here
 * instead of in `IssueDetail` keeps a toggle from re-rendering the whole
 * page (MUL-9426).
 */
function IssueDetailActionsDropdown({
  locateInSidebar,
  ...props
}: Omit<ComponentProps<typeof IssueActionsDropdown>, "onShowInSidebar"> & { locateInSidebar: boolean }) {
  const showInSidebar = useShowInSidebar(props.issue);
  return <IssueActionsDropdown {...props} onShowInSidebar={locateInSidebar ? (showInSidebar ?? undefined) : undefined} />;
}

/** The workflow the issue follows through its project (MUL-7420). */
function IssueWorkflowPropRow({
  issueId,
  projectId,
  workflowId,
  status,
  label,
}: {
  issueId: string;
  projectId: string | null;
  workflowId: string | null | undefined;
  status: string;
  label: string;
}) {
  const visible = useIssueFlowVisible(projectId, workflowId);
  if (!visible) return null;
  return (
    <PropRow label={label}>
      <IssueFlowCard issueId={issueId} projectId={projectId} workflowId={workflowId} currentStatus={status} />
    </PropRow>
  );
}
