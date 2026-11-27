package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

// Plugin guards at step entry (CHE-9779 step 4).
//
// A `plugin(<key>, <hook>)` entry gate asks a guard hook to allow or deny the
// move. The call is an HTTPS request, so it cannot run inside the write
// transaction: it would hold the issue lock for as long as the plugin takes.
// The handler calls the hook first, on a snapshot of what the verdict depends
// on, and keeps a fingerprint of that snapshot. checkEntry then recomputes the
// fingerprint under the issue lock, reading the installation row FOR SHARE, and
// consumes the verdict only when nothing changed. A difference restarts the
// write once (one more guard call); a second difference is a 409.

// errEntryGateStale: the state a guard verdict was given for changed before the
// write took its locks.
var errEntryGateStale = errors.New("the issue or a guard plugin changed while the guard was being asked")

// guardSnapshotInput is what a guard hook receives.
type guardSnapshotInput struct {
	IssueID        string                 `json:"issue_id"`
	Revision       int64                  `json:"revision"`
	From           string                 `json:"from"`
	To             string                 `json:"to"`
	StepEnteredAt  *time.Time             `json:"step_entered_at,omitempty"`
	Actor          guardSnapshotActor     `json:"actor"`
	Metadata       json.RawMessage        `json:"metadata"`
	MetadataWrites map[string]guardWriter `json:"metadata_writes"`
	Hook           string                 `json:"hook"`
	Plugin         string                 `json:"plugin"`
}

type guardSnapshotActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// guardWriter is the newest write of one declared metadata key.
type guardWriter struct {
	WriteID string             `json:"write_id"`
	Actor   guardSnapshotActor `json:"actor"`
	At      time.Time          `json:"at"`
}

// pluginGateCall is the verdict one plugin gate got before the transaction, and
// the fingerprint of the state it was given for.
type pluginGateCall struct {
	fp      string
	verdict issueworkflow.PluginVerdict
	// answered is false when the host had nothing to answer with: the
	// installation or hook is missing. The gate then fails closed.
	answered bool
}

// pluginGuardState is the pre-transaction part of a status write. A write that
// has one was prepared: a plugin gate with no call in it is stale at the lock.
type pluginGuardState struct {
	calls map[string]pluginGateCall
}

// pluginGuardInputs is everything one plugin gate's verdict depends on, read
// through one Queries handle.
type pluginGuardInputs struct {
	installation db.PluginInstallation
	found        bool
	hook         plugincontract.Hook
	hookFound    bool
	snapshot     guardSnapshotInput
	fp           string
}

// pluginGuardGates are the plugin gates of a step that are not off. A step with
// none makes no guard call.
func pluginGuardGates(step issueworkflow.Step) []issueworkflow.Guard {
	var out []issueworkflow.Guard
	for _, g := range step.Entry {
		if g.Mode == issueworkflow.EntryModeOff {
			continue
		}
		if parsed, err := issueworkflow.ParseGuard(g.Guard); err == nil && parsed.Kind == issueworkflow.GuardPlugin {
			out = append(out, parsed)
		}
	}
	return out
}

// readPluginGuardInputs reads the state a verdict depends on and fingerprints
// it. forShare locks the installation row, for the read under the issue lock.
func (h *Handler) readPluginGuardInputs(ctx context.Context, q *db.Queries, forShare bool, issue db.Issue, def *issueworkflow.Definition, step issueworkflow.Step, to string, gate issueworkflow.Guard, actor entryWrite) (pluginGuardInputs, error) {
	pluginKey, hookKey := gate.Args[0], gate.Args[1]
	in := pluginGuardInputs{}
	var err error
	if forShare {
		in.installation, err = q.GetWorkspacePluginInstallationByKeyForShare(ctx, db.GetWorkspacePluginInstallationByKeyForShareParams{WorkspaceID: issue.WorkspaceID, PluginKey: pluginKey})
	} else {
		in.installation, err = q.GetWorkspacePluginInstallationByKey(ctx, db.GetWorkspacePluginInstallationByKeyParams{WorkspaceID: issue.WorkspaceID, PluginKey: pluginKey})
	}
	switch {
	case err == nil:
		in.found = true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return in, err
	}
	if in.found {
		if hook, hookErr := service.FindHook(in.installation, hookKey); hookErr == nil && hook.Guard != nil {
			in.hook, in.hookFound = hook, true
		}
	}

	writes := map[string]string{}
	snapshotWrites := map[string]guardWriter{}
	if in.hookFound {
		for _, key := range in.hook.Guard.MetadataWriters {
			row, err := q.GetNewestMetadataWriteForKey(ctx, db.GetNewestMetadataWriteForKeyParams{IssueID: issue.ID, Key: key})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return in, err
			}
			writes[key] = uuidToString(row.ID)
			snapshotWrites[key] = guardWriter{
				WriteID: uuidToString(row.ID),
				Actor:   guardSnapshotActor{Type: row.ActorType.String, ID: uuidToString(row.ActorID)},
				At:      row.CreatedAt.Time.UTC(),
			}
		}
	}

	entered, err := stepEntered(ctx, q, issue)
	if err != nil {
		return in, err
	}
	in.snapshot = guardSnapshotInput{
		IssueID: uuidToString(issue.ID), Revision: issue.Revision, From: issue.Status, To: to,
		Actor:          guardSnapshotActor{Type: actor.actorType, ID: actor.actorID},
		Metadata:       issue.Metadata,
		MetadataWrites: snapshotWrites,
		Hook:           hookKey, Plugin: pluginKey,
	}
	if !entered.IsZero() {
		at := entered.UTC()
		in.snapshot.StepEnteredAt = &at
	}

	// The fingerprint: the issue revision, the workflow and the step's entry
	// gates as stored, the installation row and the hook's guard policy, and the
	// newest writer of each declared key. The writer id is what catches a
	// rewrite of the same value even when the revision did not move.
	fp := sha256.New()
	err = json.NewEncoder(fp).Encode(struct {
		Revision        int64
		WorkflowID      string
		WorkflowVersion time.Time
		Step            string
		Entry           []issueworkflow.EntryGate
		Found           bool
		InstallationID  string
		Enabled         bool
		Version         string
		PackageVersion  string
		UpdatedAt       time.Time
		HookKey         string
		HookFound       bool
		Guard           *plugincontract.HookGuard
		Writes          map[string]string
	}{
		issue.Revision, uuidToString(def.ID), def.Version.UTC(), step.StatusKey, step.Entry,
		in.found, uuidToString(in.installation.ID), in.installation.Enabled, in.installation.Version,
		uuidToString(in.installation.PackageVersionID), in.installation.UpdatedAt.Time.UTC(),
		hookKey, in.hookFound, in.hook.Guard, writes,
	})
	if err != nil {
		return in, err
	}
	in.fp = hex.EncodeToString(fp.Sum(nil))
	return in, nil
}

// onErrorPluginVerdict is the hook's own on_error policy, for a gate that
// reaches the lock with no guard call made for it.
func onErrorPluginVerdict(in pluginGuardInputs) (issueworkflow.PluginVerdict, bool) {
	if !in.hookFound {
		return issueworkflow.PluginVerdict{}, false
	}
	verdict := issueworkflow.PluginVerdict{Overridable: in.hook.Guard.Overridable}
	if in.hook.Guard.OnError == plugincontract.GuardOnErrorOpen {
		verdict.Allow = true
		return verdict, true
	}
	verdict.Reason = "guard plugin unavailable: " + in.snapshot.Plugin
	return verdict, true
}

// invokeGuard is the guard call; a test replaces it.
func (h *Handler) invokeGuard(ctx context.Context, invocation service.HookInvocation) (service.GuardResult, error) {
	if h.invokeGuardForTest != nil {
		return h.invokeGuardForTest(ctx, invocation)
	}
	return h.PluginService.InvokeGuard(ctx, invocation)
}

// prepareGuards makes the pre-transaction guard calls of one status write. It
// reads the issue afresh, so a restart fingerprints the state that made the
// last one stale.
func (w *workflowWrite) prepareGuards(ctx context.Context, h *Handler) error {
	if w == nil {
		return nil
	}
	w.guard = nil
	to := w.got.Status
	if w.system || !to.Valid || to.String == w.prev.Status {
		return nil
	}
	// From here the write has been prepared, even when no gate asks the plugin:
	// a plugin gate that appears before the lock has no verdict, and the write
	// restarts rather than answering from on_error without a call.
	state := &pluginGuardState{calls: map[string]pluginGateCall{}}
	w.guard = state
	issue, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: w.prev.ID, WorkspaceID: w.prev.WorkspaceID})
	if err != nil {
		return err
	}
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, w.got.ProjectID)
	if err != nil || def == nil {
		return err
	}
	step, ok := def.Step(to.String)
	if !ok {
		return nil
	}
	gates := pluginGuardGates(step)
	if len(gates) == 0 {
		return nil
	}
	for _, gate := range gates {
		in, err := h.readPluginGuardInputs(ctx, h.Queries, false, issue, def, step, to.String, gate, w.entry)
		if err != nil {
			return err
		}
		call := pluginGateCall{fp: in.fp}
		if in.found && in.hookFound {
			actor := service.HookActor{Type: w.entry.actorType}
			if id, err := util.ParseUUID(w.entry.actorID); err == nil {
				actor.ID = id
			}
			result, err := h.invokeGuard(ctx, service.HookInvocation{
				Installation: in.installation, Hook: in.hook, Actor: actor, IssueID: issue.ID, Input: in.snapshot,
			})
			if err != nil {
				slog.Warn("plugin guard call refused", "issue_id", uuidToString(issue.ID), "plugin", gate.Args[0], "hook", gate.Args[1], "error", err)
			} else {
				call.answered = true
				call.verdict = issueworkflow.PluginVerdict{
					Allow:       result.Decision == service.GuardDecisionAllow,
					Reason:      result.Reason,
					Overridable: in.hook.Guard.Overridable,
				}
			}
		}
		state.calls[issueworkflow.PluginGateKey(gate)] = call
	}
	return nil
}

// loadPluginFacts hands the step's plugin verdicts to the gate evaluation, under
// the issue lock. A verdict is used only when the fingerprint of the locked
// state equals the one it was given for. A write that made no guard call (every
// caller but the issue update) answers from the hook's on_error policy.
func (w *workflowWrite) loadPluginFacts(ctx context.Context, h *Handler, q *db.Queries, locked db.Issue, def *issueworkflow.Definition, step issueworkflow.Step, to string, facts *issueworkflow.Facts) error {
	gates := pluginGuardGates(step)
	if len(gates) == 0 {
		return nil
	}
	facts.Plugin = map[string]issueworkflow.PluginVerdict{}
	for _, gate := range gates {
		in, err := h.readPluginGuardInputs(ctx, q, true, locked, def, step, to, gate, w.entry)
		if err != nil {
			return err
		}
		key := issueworkflow.PluginGateKey(gate)
		if w.guard == nil {
			if verdict, ok := onErrorPluginVerdict(in); ok {
				facts.Plugin[key] = verdict
			}
			continue
		}
		call, ok := w.guard.calls[key]
		if !ok || call.fp != in.fp {
			return errEntryGateStale
		}
		if call.answered {
			facts.Plugin[key] = call.verdict
		}
	}
	return nil
}

// withPluginGuard runs a status write with its guard calls: the calls, then the
// write, and once more when the write found the verdicts stale.
func (h *Handler) withPluginGuard(ctx context.Context, w *workflowWrite, write func() error) error {
	for attempt := 0; ; attempt++ {
		if err := w.prepareGuards(ctx, h); err != nil {
			return err
		}
		if h.guardPreparedForTest != nil {
			h.guardPreparedForTest()
		}
		err := write()
		if attempt > 0 || !errors.Is(err, errEntryGateStale) {
			return err
		}
	}
}
