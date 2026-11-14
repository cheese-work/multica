package service

import (
	"slices"

	"github.com/multica-ai/multica/server/pkg/eventcontract"
)

const maxWakeupMatchPairs = 4

// validateSystemWakeupEvents checks where a trigger's system event types may be
// used (.plans/CHE-5696-system-events-wakeup.md, D1). A system event has no
// issue: an issue-scope rule waits on it for its own issue, and a workspace or
// project rule needs an autopilot target to run anything. `target` is the
// resolved target type, "assignee" when none is set. `server.deployed` must
// carry a `sha` or `digest` match, so a rollback or another build never wakes a
// waiter (D4).
func validateSystemWakeupEvents(events []string, scope WakeupScope, target string, match map[string]string) error {
	var system, deployed bool
	for _, e := range events {
		switch {
		case eventcontract.IsSystem(e):
			if !slices.Contains(eventcontract.SystemTypes, e) {
				return wakeupDefinitionBad("event %q is reserved and not emitted yet", e)
			}
			system = true
			deployed = deployed || e == eventcontract.EventServerDeployed
		case !slices.Contains(eventcontract.WakeupTypes, e):
			return wakeupDefinitionBad("unknown event %q", e)
		}
	}
	if !system {
		return nil
	}
	if scope != WakeupScopeIssue && target != "autopilot" {
		return wakeupDefinitionBad("a system event at workspace or project scope needs an autopilot target; an agent has no issue to run on")
	}
	if deployed && match["sha"] == "" && match["digest"] == "" {
		return wakeupDefinitionBad("a server.deployed rule must match on sha or digest")
	}
	return nil
}

// validateEffectiveSystemEvents applies validateSystemWakeupEvents to a
// resolved rule. The scope that supplies the trigger decides.
func validateEffectiveSystemEvents(eff EffectiveWakeupConfig) error {
	spec, ok := triggerSpec(eff.Config.Trigger)
	if !ok || spec.Kind != wakeupTriggerKindEvent {
		return nil
	}
	target, _ := WakeupPatchTarget(eff.Config)
	if target == "" {
		target = "assignee"
	}
	var match map[string]string
	if eff.Config.Filters.Set {
		var f wakeupFiltersSpec
		_ = decodeWakeupSpec(eff.Config.Filters.Value, &f)
		match = f.Match
	}
	return validateSystemWakeupEvents(spec.Events, eff.Sources["trigger"], target, match)
}
