"use client";

import { useState } from "react";
import { Clock, Webhook } from "@multica/ui/icons";
import { toast } from "sonner";
import { Dialog, DialogContent, DialogTitle } from "@multica/ui/components/ui/dialog";
import { Button } from "@multica/ui/components/ui/button";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCreateAutopilotTrigger } from "@multica/core/autopilots/mutations";
import {
  useAutopilotTriggerDraftStore,
  type AutopilotTriggerDraft,
} from "@multica/core/autopilots/stores";
import { usePersistedDraft } from "@multica/core/drafts";
import { ScheduleEditor } from "./schedule-editor/schedule-editor";
import { getDefaultScheduleConfig, type ScheduleConfig } from "./schedule-editor/model";
import { parseCron, toCron } from "./schedule-editor/cron-mapping";
import { useScheduleSubmitGate } from "./schedule-editor/validate";
import { browserTimezone } from "../../common/timezone-select";
import { SegmentedToggle } from "../../common/segmented-toggle";
import { useT } from "../../i18n";

const EMPTY_DRAFT: AutopilotTriggerDraft = {};

export function AddTriggerDialog({
  open,
  onOpenChange,
  autopilotId,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  autopilotId: string;
}) {
  const { t } = useT("autopilots");
  const wsId = useWorkspaceId();
  const createTrigger = useCreateAutopilotTrigger();
  // Unsent input is mirrored into the keyed draft store (one per autopilot) and
  // read back on mount; it clears only after the trigger is added.
  const [draft, patchDraft, clearDraft] = usePersistedDraft(
    useAutopilotTriggerDraftStore,
    autopilotId,
    EMPTY_DRAFT,
  );
  const [seed] = useState(draft);
  const [kind, setKindState] = useState<"schedule" | "webhook">(seed.kind ?? "schedule");
  const setKind = (v: "schedule" | "webhook") => {
    setKindState(v);
    patchDraft({ kind: v });
  };
  const [config, setConfigState] = useState<ScheduleConfig>(() =>
    seed.schedule
      ? parseCron(seed.schedule.cron, seed.schedule.timezone)
      : getDefaultScheduleConfig(browserTimezone()),
  );
  const setConfig = (cfg: ScheduleConfig) => {
    setConfigState(cfg);
    patchDraft({ schedule: { cron: toCron(cfg), timezone: cfg.timezone } });
  };
  const [label, setLabelState] = useState(seed.label ?? "");
  const setLabel = (v: string) => {
    setLabelState(v);
    patchDraft({ label: v });
  };
  const [submitting, setSubmitting] = useState(false);
  const scheduleGate = useScheduleSubmitGate(wsId);
  const canSubmit = !submitting && (kind !== "schedule" || scheduleGate.scheduleValid);

  const handleSubmit = async () => {
    if (!canSubmit) return;
    setSubmitting(true);
    try {
      if (kind === "schedule") {
        if (!(await scheduleGate.ensureAccepted(config))) {
          setSubmitting(false);
          return;
        }
        const cronExpr = toCron(config);
        if (!cronExpr.trim()) {
          setSubmitting(false);
          return;
        }
        await createTrigger.mutateAsync({
          autopilotId,
          kind: "schedule",
          cron_expression: cronExpr,
          timezone: config.timezone || undefined,
          label: label.trim() || undefined,
        });
        toast.success(t(($) => $.add_trigger_dialog.toast_added_schedule));
      } else {
        await createTrigger.mutateAsync({
          autopilotId,
          kind: "webhook",
          label: label.trim() || undefined,
        });
        toast.success(t(($) => $.add_trigger_dialog.toast_added_webhook));
      }
      clearDraft();
      onOpenChange(false);
      setKindState("schedule");
      setConfigState(getDefaultScheduleConfig(browserTimezone()));
      setLabelState("");
    } catch (err) {
      toast.error(
        err instanceof Error && err.message
          ? err.message
          : t(($) => $.add_trigger_dialog.toast_add_failed),
      );
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-sm">
        <DialogTitle>{t(($) => $.add_trigger_dialog.title)}</DialogTitle>
        {/* DialogContent is a grid, so without min-w-0 this item's min-width is
            its content's — and the cron readback is one unbreakable line that
            would push the track past the dialog instead of truncating. */}
        <div className="min-w-0 space-y-4 pt-2">
          <div>
            <label className="text-caption font-medium text-muted-foreground">
              {t(($) => $.add_trigger_dialog.type_label)}
            </label>
            <div className="mt-1">
              <SegmentedToggle
                value={kind}
                onChange={setKind}
                buttonClassName="px-3 py-1.5 text-body"
                options={[
                  [
                    "schedule",
                    <span key="schedule" className="flex items-center justify-center gap-1.5">
                      <Clock className="h-3.5 w-3.5" />
                      {t(($) => $.add_trigger_dialog.type_schedule)}
                    </span>,
                  ],
                  [
                    "webhook",
                    <span key="webhook" className="flex items-center justify-center gap-1.5">
                      <Webhook className="h-3.5 w-3.5" />
                      {t(($) => $.add_trigger_dialog.type_webhook)}
                    </span>,
                  ],
                ]}
              />
            </div>
          </div>

          {kind === "schedule" ? (
            <ScheduleEditor
              value={config}
              onChange={(next) => {
                scheduleGate.clearRejection();
                setConfig(next);
              }}
              wsId={wsId}
              onValidityChange={scheduleGate.onValidityChange}
              // Same reason as the autopilot dialog: the submit path reads the
              // schedule, validates it over the network, then writes what it
              // read — an edit landing inside that window would be discarded.
              disabled={submitting}
            />
          ) : (
            <p className="rounded-md bg-muted/50 px-3 py-2 text-caption text-muted-foreground">
              {t(($) => $.add_trigger_dialog.webhook_help)}
            </p>
          )}

          <div>
            <label className="text-caption font-medium text-muted-foreground">
              {t(($) => $.add_trigger_dialog.label_field)}
            </label>
            <input
              type="text"
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              placeholder={t(($) => $.add_trigger_dialog.label_placeholder)}
              className="mt-1 w-full rounded-md border bg-background px-3 py-2 text-body outline-none focus:ring-1 focus:ring-ring"
            />
          </div>
          <div className="flex justify-end pt-1">
            <Button size="sm" onClick={handleSubmit} disabled={!canSubmit}>
              {submitting
                ? t(($) => $.add_trigger_dialog.submitting)
                : t(($) => $.add_trigger_dialog.submit)}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
