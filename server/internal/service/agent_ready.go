package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/dispatch"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AgentAvailability is what a readiness check concluded, in the vocabulary the
// callers actually branch on.
//
// The distinction that matters is not "ready or not" but whether WAITING is a
// plan. A sleeping laptop comes back on its own, so queued work is right to
// wait for it; an agent bound to nothing, or bound to a machine whose CLI
// cannot run, will never pick that work up until a human acts. dispatch's
// reason codes already draw that line — this type is the server-side half so
// every admission path draws it the same way.
type AgentAvailability int

const (
	// AgentAvailable: the agent can take work now.
	AgentAvailable AgentAvailability = iota
	// AgentWaitable: not runnable right now, but nothing is broken — the
	// machine is offline and work queued for it runs when it returns.
	AgentWaitable
	// AgentBlocked: nothing will claim this agent's work until someone
	// intervenes. Callers must refuse the trigger and say why.
	AgentBlocked
)

// AgentVerdict is a readiness decision plus everything a caller needs to act on
// it: the wire reason code, and — when the daemon reported one — the command
// that repairs the runtime.
type AgentVerdict struct {
	Availability AgentAvailability
	// Reason is the dispatch code for a non-available verdict, empty otherwise.
	Reason dispatch.ReasonCode
	// Repair is the fix the daemon reported for an unusable runtime: the npm
	// package that owns the broken entry point and the command that reinstalls
	// it. Absent for every other verdict, and for a runtime whose daemon is too
	// old to report one.
	Repair *RuntimeRepair
	// Detail is the daemon's own description, for logs and for the record left
	// on a blocked non-interactive trigger. Never parsed.
	Detail string
}

// Ready reports whether the agent can take work right now.
func (v AgentVerdict) Ready() bool { return v.Availability == AgentAvailable }

// Blocked reports whether waiting is futile and the caller must refuse the
// trigger rather than queue it.
func (v AgentVerdict) Blocked() bool { return v.Availability == AgentBlocked }

// RuntimeRepair mirrors the daemon's agent.ExecFormatRepair over the wire.
type RuntimeRepair struct {
	Package string `json:"package,omitempty"`
	Command string `json:"command,omitempty"`
	// Shell is the interpreter Command is written for, as reported by the
	// machine that must run it ("bash", "powershell"). Rendered as the code
	// block's language so a Windows user is not handed POSIX syntax in a bash
	// fence. Empty from a daemon too old to report it.
	Shell string `json:"shell,omitempty"`
}

// runtimeOfflineReason is the daemon's structured explanation, stored on the
// runtime row's metadata by the deregister handler.
type runtimeOfflineReason struct {
	Code   string         `json:"code"`
	Detail string         `json:"detail"`
	Repair *RuntimeRepair `json:"repair"`
	// Installing: the daemon has an automatic install in flight, so this offline
	// runtime is expected to come back on its own and work may queue.
	Installing bool `json:"installing"`
}

// runtimeOfflineCodeNotExecutable is the daemon's code for "the OS refuses to
// execute this agent CLI" (daemon.RuntimeOfflineCodeNotExecutable). Compared as
// a string because the server must not import the daemon package.
const runtimeOfflineCodeNotExecutable = "not_executable"

// runtimeOfflineCodeDshProfile is the daemon's code for "this runtime's DSH
// runtime profile is not installed" (daemon.RuntimeOfflineCodeDshProfile). Same
// string-comparison rule as above, and the same MUL-6164 semantics: with no
// install configured the wait never resolves, so the trigger is refused.
// runtimeOfflineReason.Installing is the exception — a daemon that is installing
// the profile right now resolves it by itself.
const runtimeOfflineCodeDshProfile = "dsh_profile"

// RuntimeBlockedNeedsNotice reports whether a blocked verdict's cause is one a
// human has to act on OFF the platform — either repairing the runtime's own
// machine, or lifting a workspace policy hold only a workspace owner can
// touch — and therefore one that must leave a durable explanation on the
// issue (MUL-6164, CHE-588).
//
// A predicate rather than an inline comparison because three admission paths
// ask it — the refused @mention, the refused assignment, and the refused
// assign-on-create — and a code added to one of them but not the others is a
// trigger that vanishes with no trace on exactly the surfaces that have no
// response for the user to read. ReasonProviderHold is exactly that case: the
// dispatch that CHE-588 added to catch a held provider ahead of the network
// call is silent on all three of these paths unless its code is listed here
// too, which reproduces on a policy hold the same blind spot MUL-6164 closed
// for a broken runtime — a refusal with no visible reason is, if anything,
// worse than the failure it replaced.
func RuntimeBlockedNeedsNotice(code dispatch.ReasonCode) bool {
	return code == dispatch.ReasonRuntimeUnusable ||
		code == dispatch.ReasonRuntimeProfileMissing ||
		code == dispatch.ReasonProviderHold ||
		code == dispatch.ReasonRuntimeAccessDenied
}

// AgentReadiness reports whether an agent can accept new work right now, and
// what the caller should do when it cannot.
//
// The lookup carries the connection to read on plus the source label the
// runtime read is attributed to (MUL-6884), so each admission path stays
// distinguishable in multica_agent_runtime_lookup_total. The same connection
// (lookup.Queries) also backs the provider-hold check below — no new
// plumbing, and no signature change at any of this function's 12+ call
// sites, all of which already construct a RuntimeLookup with Queries set.
//
// err is non-nil only on DB lookup failure for the runtime row, or, for the
// same "could not read the source of truth" reason, a GetWorkspace error
// other than pgx.ErrNoRows for the workspace row the provider-hold check
// needs (see providerHoldVerdict below). Callers that treat a transient DB
// error as "do not skip" (the autopilot admission gate) should swallow it;
// callers that need a hard yes/no (the squad-leader pre-enqueue check in the
// handler) should fail closed. A malformed (as opposed to unreadable) hold
// configuration is NOT surfaced as err — see providerHoldVerdict below — so
// it cannot be swallowed by a caller that treats DB errors as "proceed
// anyway".
//
// This is the single source of truth shared by:
//   - service.shouldSkipDispatch (autopilot admission gate)
//   - service.dispatchRunOnly    (squad-leader runtime check, MUL-2429)
//   - handler.isSquadLeaderReady (issue-assign / comment-trigger path)
//   - the direct-agent trigger paths, which consult it for the BLOCKED verdict
//     only: an offline machine still queues, because that wait ends by itself.
//
// Keeping these aligned matters because the paths can otherwise drift — e.g.
// one starts allowing "starting" runtimes while another doesn't, and the bug
// only surfaces when a user assigns the same agent through two different entry
// points. Touch this function, all of them move together.
//
// Scope: this gates every TRIGGER admission path — assignment, @mention,
// comment-trigger, chat, autopilot dispatch. It does NOT run on the retry or
// claim paths (server/internal/service/task.go has no call to this
// function): a task already admitted before a hold was configured, which
// then fails with a transient provider error, is re-queued by
// retryableReasons without a fresh readiness consult, and the same is true of
// already-queued work a daemon claims. A provider hold configured after
// in-flight work exists therefore does not retroactively stop that work from
// reaching the held provider again on retry — a known gap, deferred to a
// follow-up (CHE-588).
func AgentReadiness(ctx context.Context, lookup RuntimeLookup, agent db.Agent) (AgentVerdict, error) {
	if agent.ArchivedAt.Valid {
		return AgentVerdict{
			Availability: AgentBlocked,
			Reason:       dispatch.ReasonTargetUnavailable,
			Detail:       "agent is archived",
		}, nil
	}
	// Provider-hold check comes before the runtime-required / runtime-lookup
	// checks below, on purpose (CHE-588): policy precedes availability. An
	// agent bound to nothing and an agent bound to a perfectly healthy,
	// online runtime must both be refused the same way when their model's
	// provider is on hold — the point of gating here, ahead of
	// lookup.Get(ctx, agent.RuntimeID), is that the network path to the
	// held provider is never reached at all, not merely reached and then
	// aborted.
	//
	// A non-nil err here is a real GetWorkspace failure (not the "no row"
	// case, which providerHoldVerdict already resolves to a plain fall-
	// through) and is propagated rather than swallowed into ALLOW: a
	// transient Postgres hiccup must not be silently treated as "no hold
	// applies", because that is indistinguishable, at the call site, from
	// the hold genuinely not existing — the exact silent-passthrough defect
	// CHE-588 was filed to eliminate, just moved one layer down from
	// malformed settings to an unreadable workspace row. This function's own
	// contract (see the doc comment above) already delegates the choice of
	// what to do with that error to each caller: the autopilot admission
	// gate swallows it and proceeds, the handler's hard-gate pre-enqueue
	// check fails closed. Blanket fail-closed HERE would instead refuse
	// every dispatch workspace-wide on one flaky read, which is a worse
	// outage than the one this feature exists to prevent.
	verdict, blocked, err := providerHoldVerdict(ctx, lookup, agent)
	if err != nil {
		return AgentVerdict{}, err
	}
	if blocked {
		return verdict, nil
	}
	if !agent.RuntimeID.Valid {
		return AgentVerdict{
			Availability: AgentBlocked,
			Reason:       dispatch.ReasonAgentRuntimeRequired,
			Detail:       "agent has no runtime bound",
		}, nil
	}
	rt, err := lookup.Get(ctx, agent.RuntimeID)
	if err != nil {
		return AgentVerdict{}, err
	}
	return runtimeVerdict(rt, agent), nil
}

// providerHoldVerdict is the provider-hold half of AgentReadiness, split out
// so it is testable without a runtime row and so the ordering rationale above
// stays next to one call instead of buried in a long function body.
//
// blocked is false whenever the agent may proceed to the runtime checks:
// no Queries to read a workspace with (test callers passing RuntimeLookup{}
// for the agent-only cases in TestAgentReadinessVerdict), no workspace row
// (pgx.ErrNoRows — see below), no configured holds, the agent's model not
// resolving to a known provider (ResolveModelProvider's ok=false — see its
// doc comment on why an unresolved model must never block), or the resolved
// provider simply not being held. Every one of those is "cannot evaluate a
// hold" or "no hold applies", and all must fall through to the ordinary
// runtime checks, not fail closed — fail-closed in this feature is reserved
// for a settings blob that IS present and IS malformed, handled below, and
// (via the returned error) for a GetWorkspace failure that is NOT "no row".
//
// err is non-nil only for a GetWorkspace failure other than pgx.ErrNoRows —
// see the fall-through below for why "no row" is not folded into it.
func providerHoldVerdict(ctx context.Context, lookup RuntimeLookup, agent db.Agent) (AgentVerdict, bool, error) {
	if lookup.Queries == nil {
		return AgentVerdict{}, false, nil
	}
	ws, err := lookup.Queries.GetWorkspace(ctx, agent.WorkspaceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No workspace row to read a hold from (deleted workspace, or —
			// far more likely in tests — a fixture that never inserted one).
			// This is not the fail-closed case: fail-closed is for settings
			// that exist and do not parse, not for "there is nothing to read
			// yet", so fall through to the ordinary runtime checks exactly
			// as if no hold were configured.
			return AgentVerdict{}, false, nil
		}
		// Any other error is a real "could not read the source of truth"
		// failure (a Postgres hiccup, a dropped connection) — not "there is
		// nothing to read". Propagate it out of AgentReadiness as err rather
		// than falling through to ALLOW: this function's doc comment already
		// promises err covers the workspace row this check needs, and
		// falling through here would make a transient DB error
		// indistinguishable from "no hold applies", which is exactly the
		// silent-passthrough defect CHE-588 exists to eliminate — just moved
		// from malformed settings to an unreadable row. This is NOT blanket
		// fail-closed, though: a Postgres blip must not refuse every
		// dispatch workspace-wide, so the decision is left to each caller of
		// AgentReadiness, which already has its own policy for a DB error
		// (autopilot swallows it and proceeds; the handler's hard-gate fails
		// closed).
		return AgentVerdict{}, false, err
	}
	holds, err := ProviderHoldsFromSettings(ws.Settings)
	if err != nil {
		// Fail CLOSED: malformed settings JSON must refuse the agent, not
		// silently behave as if no hold were configured. See
		// ProviderHoldsFromSettings's doc comment for why this codebase's
		// usual fail-open-on-malformed-settings convention is wrong here.
		// This is a BLOCKED verdict, not a Go error, on purpose — see this
		// function's doc comment above.
		return AgentVerdict{
			Availability: AgentBlocked,
			Reason:       dispatch.ReasonProviderHold,
			Detail:       fmt.Sprintf("workspace provider-hold settings could not be parsed: %v", err),
		}, true, nil
	}
	if len(holds) == 0 {
		return AgentVerdict{}, false, nil
	}
	provider, ok := ResolveModelProvider(agent.Model.String)
	if !ok {
		return AgentVerdict{}, false, nil
	}
	hold, held := findProviderHold(holds, provider)
	if !held {
		return AgentVerdict{}, false, nil
	}
	notice := ProviderHoldNotice(agent.Name, provider, hold, providerHoldSubstitutes(ctx, lookup, agent, holds))
	return AgentVerdict{
		Availability: AgentBlocked,
		Reason:       dispatch.ReasonProviderHold,
		Detail:       notice,
	}, true, nil
}

// providerHoldSubstitutes lists other agents in the same workspace whose
// model does not resolve to a held provider, for ProviderHoldNotice's
// "try one of these instead" line.
//
// Never lets a lookup failure turn a refusal into an allow: this is called
// only from inside providerHoldVerdict's already-decided BLOCKED path, so a
// ListAgents error or an empty result simply omits the substitute list — the
// caller still returns AgentBlocked either way. archived_at IS NULL and
// kind = 'user' are already applied by the ListAgents query itself.
func providerHoldSubstitutes(ctx context.Context, lookup RuntimeLookup, agent db.Agent, holds []ProviderHold) []string {
	agents, err := lookup.Queries.ListAgents(ctx, agent.WorkspaceID)
	if err != nil {
		return nil
	}
	var names []string
	for _, candidate := range agents {
		if candidate.ID == agent.ID {
			continue
		}
		provider, ok := ResolveModelProvider(candidate.Model.String)
		if ok {
			if _, held := findProviderHold(holds, provider); held {
				continue
			}
		}
		names = append(names, candidate.Name)
	}
	return names
}

// runtimeVerdict combines runtime health with the ownership binding that
// determines whether this agent can execute there.
func runtimeVerdict(rt db.AgentRuntime, agent db.Agent) AgentVerdict {
	if rt.Visibility == "private" && rt.OwnerID.Valid && (!agent.OwnerID.Valid || agent.OwnerID != rt.OwnerID) {
		return AgentVerdict{
			Availability: AgentBlocked,
			Reason:       dispatch.ReasonRuntimeAccessDenied,
			Detail:       "agent owner does not match private runtime owner",
		}
	}
	if rt.Status == "online" {
		return AgentVerdict{Availability: AgentAvailable}
	}
	// Offline with a reason the daemon says a human must repair: refuse rather
	// than queue, and carry the repair so the caller can show it.
	if reason, ok := parseRuntimeOfflineReason(rt.Metadata); ok {
		switch {
		case reason.Code == runtimeOfflineCodeNotExecutable:
			return AgentVerdict{
				Availability: AgentBlocked,
				Reason:       dispatch.ReasonRuntimeUnusable,
				Repair:       reason.Repair,
				Detail:       reason.Detail,
			}
		// A missing runtime profile is the same shape of finding — the machine
		// is reachable and its CLI cannot serve work — but its own reason code,
		// because the CLI is not the broken part and "reinstall the CLI" copy
		// would send the user to the wrong repair. The one exception is an
		// install the daemon is running right now: that wait DOES end by
		// itself, so the work queues instead of being refused.
		case reason.Code == runtimeOfflineCodeDshProfile && !installClaimHolds(reason, rt):
			return AgentVerdict{
				Availability: AgentBlocked,
				Reason:       dispatch.ReasonRuntimeProfileMissing,
				Repair:       reason.Repair,
				Detail:       reason.Detail,
			}
		}
	}
	return AgentVerdict{
		Availability: AgentWaitable,
		Reason:       dispatch.ReasonRuntimeOffline,
		Detail:       "agent runtime is " + rt.Status,
	}
}

// runtimeInstallClaimWindow is how long an `installing` claim on a runtime row
// is honoured before the row is read as a plain missing profile.
//
// The claim is what makes this one offline runtime worth queueing behind, so a
// claim that outlives its install recreates exactly the failure the structured
// reason exists to prevent — work queued behind something that stopped. The
// daemon withdraws the claim when an install gives up, but a withdraw cannot be
// guaranteed: the verdict is built before the runtime ids the withdraw needs are
// known, the corrective Deregister is best-effort and runs on an already-
// cancelled context during shutdown, and a daemon restarted mid-install does not
// track the row at all.
//
// So the claim is bounded rather than guaranteed. SetAgentRuntimeOfflineWithReason
// stamps updated_at, and the install it describes is itself bounded by the
// daemon's provisioning timeout; a window comfortably longer than that covers a
// real install while capping every way the claim can be left behind. The
// withdraw stays as the fast path — this is the floor under it.
const runtimeInstallClaimWindow = 5 * time.Minute

// installClaimHolds reports whether a runtime row's in-flight-install claim is
// still worth queueing behind.
//
// A row with no usable timestamp cannot be bounded, so the claim is not honoured:
// the cost of refusing during a real install is one retry the notice already
// asks for, and the cost of honouring a stale claim is work that queues until it
// expires.
func installClaimHolds(reason runtimeOfflineReason, rt db.AgentRuntime) bool {
	if !reason.Installing || !rt.UpdatedAt.Valid {
		return false
	}
	return time.Since(rt.UpdatedAt.Time) < runtimeInstallClaimWindow
}

// parseRuntimeOfflineReason reads the daemon's explanation off a runtime row.
// A row with no reason, or metadata this server cannot parse, simply has no
// explanation — never a different verdict.
func parseRuntimeOfflineReason(metadata []byte) (runtimeOfflineReason, bool) {
	if len(metadata) == 0 {
		return runtimeOfflineReason{}, false
	}
	var envelope struct {
		OfflineReason *runtimeOfflineReason `json:"offline_reason"`
	}
	if err := json.Unmarshal(metadata, &envelope); err != nil || envelope.OfflineReason == nil {
		return runtimeOfflineReason{}, false
	}
	return *envelope.OfflineReason, true
}

// RuntimeUnusableNotice is the durable explanation left on an issue when a
// trigger is refused with no other response the user reads — the single
// rendering point all three RuntimeBlockedNeedsNotice call sites share (the
// refused @mention, the refused assignment, and the refused assign-on-create).
//
// It lives here, next to the verdict, because two layers write it: the handler
// for a refused @mention and the service for a refused assignment. One text,
// one place to fix it. For the two runtime causes, it names the repair
// command when the daemon reported one and stays useful when it did not — a
// natively installed CLI has no postinstall to re-run, and inventing a
// command would send the user somewhere that does not exist.
//
// The text is chosen by the verdict's reason, not by whether a repair command
// happened to be present. The causes have DIFFERENT repairs, and confusing
// them sends the user to fix the wrong thing: an unrunnable CLI is
// reinstalled; a CLI missing its runtime profile is working perfectly and
// reinstalling it changes nothing; a provider hold is a workspace policy
// decision that no machine-side repair touches at all. One runtime text for
// both runtime causes already told DSH users to reinstall a CLI that was
// never the problem (MUL-6164) — rendering that same runtime-repair copy for
// a provider hold would reproduce the identical misdiagnosis one layer up,
// telling a user to reinstall a healthy CLI when the real cause is a policy
// hold only a workspace owner can lift (CHE-588).
func RuntimeUnusableNotice(agentName string, verdict AgentVerdict) string {
	name := agentName
	if name == "" {
		name = "The assigned agent"
	}
	// A provider hold's full text — including the policy text and any
	// substitute agents — is already built by providerHoldVerdict at the
	// point the hold is discovered (it has the workspace's ProviderHold and
	// substitute list right there; rebuilding it here would need both again).
	// verdict.Detail carries that finished notice verbatim, so this is a
	// pass-through, not a second renderer to keep in sync with the first.
	if verdict.Reason == dispatch.ReasonProviderHold {
		return verdict.Detail
	}
	if verdict.Reason == dispatch.ReasonRuntimeAccessDenied {
		return fmt.Sprintf(
			"%s cannot run on this private runtime, so this trigger was not queued. Make the runtime's machine public, or rebind/copy the agent to a runtime its owner can use.",
			name,
		)
	}
	if verdict.Reason == dispatch.ReasonRuntimeProfileMissing {
		return runtimeProfileMissingNotice(name)
	}
	if verdict.Repair != nil && verdict.Repair.Command != "" {
		return fmt.Sprintf(
			"%s could not start: its CLI is installed but cannot be executed on that machine, so this trigger was not queued.\n\n"+
				"Usually the package's postinstall was blocked (npm 12 allowScripts, pnpm 10 approve-builds, `--ignore-scripts`, `--omit=optional`) and the bin entry is still a placeholder. On that machine, run:\n\n"+
				"```%s\n%s\n```\n\n"+
				"The runtime comes back on its own within a couple of minutes; trigger the agent again after that.",
			name, repairFenceLanguage(verdict.Repair.Shell), verdict.Repair.Command,
		)
	}
	return fmt.Sprintf(
		"%s could not start: its CLI is installed but cannot be executed on that machine, so this trigger was not queued. "+
			"Reinstall the agent CLI on that machine with install scripts enabled; the runtime comes back on its own within a couple of minutes.",
		name,
	)
}

// runtimeProfileMissingNotice explains a runtime whose CLI runs but whose
// Multica runtime profile is absent.
//
// No fenced command, deliberately. The install is `dsh plugin --profile multica
// add <bundle>`, and <bundle> is the operator's own choice of package,
// directory or tarball — Multica's bridge is not on a public registry yet
// (multica#6936). Rendering that line in a code block presents a placeholder as
// something to copy and run, which is the shape of instruction people paste
// verbatim and then report as broken. Naming the two ways to supply a real
// bundle, and pointing at the docs that list them, is the honest version.
func runtimeProfileMissingNotice(name string) string {
	return fmt.Sprintf(
		"%s could not start: the DeepSeek Harness CLI is installed on that machine, but the `multica` runtime profile it needs is not, so this trigger was not queued.\n\n"+
			"The profile supplies the protocol Multica drives — the CLI itself is fine, and reinstalling it changes nothing. On that machine, either add the Multica DSH runtime bundle to the profile with `dsh plugin --profile multica add`, or set `MULTICA_DSH_PROFILE_BUNDLE` for the daemon so it installs the bundle itself. See the agent runtime install docs for the bundle to use.\n\n"+
			"The runtime registers on its own within a couple of minutes after that; trigger the agent again then.",
		name,
	)
}

// repairFenceLanguage labels the code block with the shell the command was
// written for. A daemon too old to report one predates the Windows rendering
// entirely, so its command is POSIX by construction.
func repairFenceLanguage(shell string) string {
	if shell == "powershell" {
		return "powershell"
	}
	return "bash"
}
