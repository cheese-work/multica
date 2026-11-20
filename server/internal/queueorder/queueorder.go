// Package queueorder holds the two workspace settings that choose how queued
// runs are ordered (CHE-8749): queue_order and queue_aging_minutes. Unset keys
// read as their defaults, which keep today's order.
package queueorder

import (
	"fmt"
	"strings"
)

const (
	KeyOrder = "queue_order"
	KeyAging = "queue_aging_minutes"

	ModePriority    = "priority"
	ModeProjectRank = "project_rank"

	DefaultAgingMinutes = 30
	MinAgingMinutes     = 5
	MaxAgingMinutes     = 720
)

// Modes lists the allowed queue_order values; the first is the default.
var Modes = []string{ModePriority, ModeProjectRank}

// Order returns the effective queue_order of a settings object.
func Order(settings map[string]any) string {
	if mode, ok := settings[KeyOrder].(string); ok && mode == ModeProjectRank {
		return ModeProjectRank
	}
	return ModePriority
}

// AgingMinutes returns the effective queue_aging_minutes of a settings object.
func AgingMinutes(settings map[string]any) int {
	if n, ok := wholeNumber(settings[KeyAging]); ok && n >= MinAgingMinutes && n <= MaxAgingMinutes {
		return n
	}
	return DefaultAgingMinutes
}

// ValidateSetting checks the two keys of a settings object. A missing key
// passes; any present value, null included, must be allowed. A value that is
// not an object has no keys, so the caller's own checks apply.
func ValidateSetting(value any) error {
	settings, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if v, present := settings[KeyOrder]; present {
		if mode, _ := v.(string); mode != ModePriority && mode != ModeProjectRank {
			return fmt.Errorf("%s must be one of %s, got %v", KeyOrder, strings.Join(Modes, ", "), v)
		}
	}
	if v, present := settings[KeyAging]; present {
		if n, ok := wholeNumber(v); !ok || n < MinAgingMinutes || n > MaxAgingMinutes {
			return fmt.Errorf("%s must be a whole number from %d to %d, got %v", KeyAging, MinAgingMinutes, MaxAgingMinutes, v)
		}
	}
	return nil
}

func wholeNumber(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}
