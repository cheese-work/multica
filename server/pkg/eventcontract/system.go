package eventcontract

import "slices"

// System events are server facts with no issue: a runtime comes or goes, a
// server boots healthy, an autopilot run is skipped or fails. They are kept out
// of WakeupTypes (issue facts with transactional capture) and carry the
// `system` mark through SystemTypes and IsSystem. See
// .plans/CHE-5696-system-events-wakeup.md, D1.
const (
	EventRuntimeOnline    = "runtime.online"
	EventRuntimeOffline   = "runtime.offline"
	EventServerDeployed   = "server.deployed"
	EventAutopilotSkipped = "autopilot.skipped"
	EventAutopilotFailed  = "autopilot.failed"
	// EventDaemonRestarted is reserved: marked system, never emitted or accepted.
	EventDaemonRestarted = "daemon.restarted"
)

// SystemTypes lists the system events a rule may select today.
var SystemTypes = []string{
	EventRuntimeOnline, EventRuntimeOffline, EventServerDeployed, EventAutopilotSkipped, EventAutopilotFailed,
}

// IsSystem reports whether the event type is a system event, reserved ones included.
func IsSystem(event string) bool {
	return event == EventDaemonRestarted || slices.Contains(SystemTypes, event)
}
