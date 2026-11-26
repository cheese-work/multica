package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

// Guard decisions and where they came from.
const (
	GuardDecisionAllow = "allow"
	GuardDecisionDeny  = "deny"

	// GuardSourcePlugin: the plugin answered. GuardSourceOnError: it did not (or
	// was not asked), and the hook's guard.on_error policy decided.
	GuardSourcePlugin  = "plugin"
	GuardSourceOnError = "on_error"

	// guardMaxReasonLength bounds a deny reason, which is shown to the member
	// whose move was refused.
	guardMaxReasonLength = 500
)

// GuardResult is the outcome of one guard call. Decision is always allow or
// deny: a failed call is already folded through on_error, so the caller never
// has to interpret transport state.
type GuardResult struct {
	Decision string
	Reason   string
	Source   string
	// Status is the invocation status: ok, failed, timeout or refused.
	Status string
	// Error is the host's own description of a failure, never the plugin's text.
	Error        string
	LatencyMs    int
	InvocationID pgtype.UUID
	HookKey      string
	// Called is false when the host answered from on_error without a network
	// call (disabled installation, open breaker, rate limit).
	Called bool
}

type guardAnswer struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// InvokeGuard asks a guard hook to allow or deny one write and waits at most
// guard.timeout_ms for the answer.
//
// It reuses callHookEndpoint, so signing, the net: scope, the public HTTPS rule
// and the per-hook limits are the ones every other trigger already gets. The
// callback token minted for the call carries the guard trigger, which the
// Action API treats as read-only.
//
// An error is returned only for a caller mistake (a hook that is not a guard).
// Everything the plugin or the network can do wrong — error, timeout, 5xx, a
// malformed answer, an open breaker — becomes a result decided by on_error.
func (s *PluginService) InvokeGuard(ctx context.Context, invocation HookInvocation) (GuardResult, error) {
	hook := invocation.Hook
	if hook.Guard == nil || !HookAllowsTrigger(hook, plugincontract.TriggerGuard) {
		return GuardResult{}, pluginErrf(PluginErrorForbidden, "hook %q is not a guard hook", hook.Key)
	}
	if !invocation.ID.Valid {
		invocation.ID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	}
	invocation.Trigger = plugincontract.TriggerGuard
	invocation.Attempt = 1
	// The guard's own bound replaces the hook default; validation already
	// refuses a guard hook that also sets timeout_ms.
	hook.TimeoutMs = hook.Guard.TimeoutMs
	invocation.Hook = hook

	result := GuardResult{InvocationID: invocation.ID, HookKey: hook.Key}
	onError := func(status, message string) GuardResult {
		result.Status = status
		result.Error = message
		result.Source = GuardSourceOnError
		if hook.Guard.OnError == plugincontract.GuardOnErrorOpen {
			result.Decision = GuardDecisionAllow
			return result
		}
		result.Decision = GuardDecisionDeny
		result.Reason = "guard plugin unavailable: " + guardPluginKey(invocation)
		return result
	}

	// None of these make a call, so none leave an invocation row: a refused row
	// would feed the breaker that refused it and hold it open for as long as
	// traffic keeps arriving.
	if !invocation.Installation.Enabled {
		return onError("refused", "this Plugin is disabled"), nil
	}
	if s.HookBreakerOpen(ctx, invocation.Installation.ID, hook.Key) {
		return onError("refused", "guard circuit breaker is open"), nil
	}
	if err := s.checkHookRate(ctx, invocation.Installation.ID, hook.Key); err != nil {
		return onError("refused", redactHookError(err)), nil
	}

	started := time.Now()
	output, callErr := s.callHookEndpoint(ctx, invocation)
	result.Called = true
	result.LatencyMs = int(time.Since(started).Milliseconds())

	if callErr != nil {
		status := hookFailureStatus(callErr)
		message := redactHookError(callErr)
		s.recordInvocation(ctx, invocation, status, 1, result.LatencyMs, message)
		return onError(status, message), nil
	}
	answer, err := parseGuardAnswer(output)
	if err != nil {
		s.recordInvocation(ctx, invocation, "failed", 1, result.LatencyMs, err.Error())
		return onError("failed", err.Error()), nil
	}
	s.recordInvocation(ctx, invocation, "ok", 1, result.LatencyMs, "")
	result.Status = "ok"
	result.Source = GuardSourcePlugin
	result.Decision = answer.Decision
	result.Reason = answer.Reason
	return result, nil
}

// parseGuardAnswer is strict about the two things a verdict must be: a known
// decision, and for a deny a reason the member can read.
func parseGuardAnswer(output json.RawMessage) (guardAnswer, error) {
	var answer guardAnswer
	if len(output) == 0 {
		return answer, errors.New("guard answer is empty")
	}
	if err := json.Unmarshal(output, &answer); err != nil {
		return guardAnswer{}, errors.New("guard answer is malformed")
	}
	switch answer.Decision {
	case GuardDecisionAllow:
		answer.Reason = ""
	case GuardDecisionDeny:
		answer.Reason = strings.TrimSpace(answer.Reason)
		if answer.Reason == "" {
			return guardAnswer{}, errors.New("guard deny has no reason")
		}
		if utf8.RuneCountInString(answer.Reason) > guardMaxReasonLength {
			return guardAnswer{}, errors.New("guard deny reason is over 500 characters")
		}
	default:
		return guardAnswer{}, errors.New("guard answer has an unknown decision")
	}
	return answer, nil
}

// guardPluginKey names the plugin in an on_error refusal, falling back to the
// hook key when the stored manifest cannot be read.
func guardPluginKey(invocation HookInvocation) string {
	if manifest, err := ParseInstallationManifest(invocation.Installation); err == nil && manifest.Key != "" {
		return manifest.Key
	}
	return invocation.Hook.Key
}
