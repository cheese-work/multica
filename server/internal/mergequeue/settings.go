package mergequeue

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Workspace setting keys (plan 4.6).
const (
	RoutingKey       = "merge_queue_dequeue_routing"
	ActorBindingsKey = "merge_queue_actor_bindings"
	WatchIssueKey    = "merge_queue_watch_issue"
)

// Keys lists the three keys, for write paths that need the set.
var Keys = []string{RoutingKey, ActorBindingsKey, WatchIssueKey}

// ErrRoutingNotAvailable is the refusal text for routing "block". B11 replaces
// it with the rollout gate.
const ErrRoutingNotAvailable = "routing-not-available"

// ValidateSetting checks the merge-queue keys of a workspace settings object.
// Every write path calls it: the owner flag, the generic settings save and the
// config propose action. A value that is not an object has no keys, so it is
// left to the caller's own checks.
func ValidateSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if v, present := config[RoutingKey]; present {
		if err := validateRouting(v); err != nil {
			return err
		}
	}
	if v, present := config[ActorBindingsKey]; present {
		if err := validateBindings(v); err != nil {
			return err
		}
	}
	if v, present := config[WatchIssueKey]; present {
		if err := validateWatchIssue(v); err != nil {
			return err
		}
	}
	return nil
}

func validateRouting(v any) error {
	switch s, _ := v.(string); s {
	case "off", "warn":
		return nil
	case "block":
		return fmt.Errorf("%s: %s", RoutingKey, ErrRoutingNotAvailable)
	}
	return fmt.Errorf("%s must be off or warn", RoutingKey)
}

func validateBindings(v any) error {
	bad := fmt.Errorf("%s must be an object with agents and people lists of logins", ActorBindingsKey)
	obj, ok := v.(map[string]any)
	if !ok {
		return bad
	}
	seen := map[string]string{} // lower-cased login -> list it came from
	for _, list := range []string{"agents", "people"} {
		raw, present := obj[list]
		if !present {
			continue
		}
		items, ok := raw.([]any)
		if !ok {
			return bad
		}
		for _, item := range items {
			login, ok := item.(string)
			login = strings.ToLower(strings.TrimSpace(login))
			if !ok || login == "" {
				return bad
			}
			if other, dup := seen[login]; dup && other != list {
				return fmt.Errorf("%s: login %q is in both the agents and people lists", ActorBindingsKey, login)
			}
			seen[login] = list
		}
	}
	for name := range obj {
		if name != "agents" && name != "people" {
			return bad
		}
	}
	return nil
}

func validateWatchIssue(v any) error {
	s, ok := v.(string)
	if ok && s == "" {
		return nil
	}
	if _, err := uuid.Parse(s); !ok || err != nil {
		return fmt.Errorf("%s must be an issue id or empty", WatchIssueKey)
	}
	return nil
}
