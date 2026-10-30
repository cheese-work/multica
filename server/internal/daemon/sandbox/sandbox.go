// Package sandbox is the daemon's run launcher core (CHE-2216 slice A2a): it
// wraps an agent command in an unprivileged bubblewrap sandbox whose mounts
// are built from an allowlist, inside a per-run child cgroup. Anything not
// named in a Spec is absent from the sandbox, not denied.
package sandbox

import (
	"fmt"
	"strings"
)

// Mode is the per-runtime enforcement flag.
type Mode string

const (
	// ModeOff runs commands exactly as before.
	ModeOff Mode = "off"
	// ModeAudit runs commands as before and logs what ModeOn would refuse.
	ModeAudit Mode = "audit"
	// ModeOn sandboxes every command and refuses to run when it cannot.
	ModeOn Mode = "on"
)

// ParseMode maps a flag value to a Mode. The empty string means off.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "", ModeOff:
		return ModeOff, nil
	case ModeAudit, ModeOn:
		return m, nil
	default:
		return "", fmt.Errorf("sandbox: unknown enforcement mode %q (want off, audit or on)", s)
	}
}
