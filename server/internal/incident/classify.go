// Package incident turns a burst of failed agent runs that share one provider
// error class into one incident: detect it, open one issue, plan where the
// affected work goes, notify once, close it when the class goes quiet (CHE-9892).
//
// The package is pure: it takes run history and returns actions. Reading runs
// and executing the actions is the caller's job, so the 25/11 replay test needs
// no database.
package incident

import (
	"strings"

	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// Class is the provider error class one incident covers. One open incident per class.
type Class string

const (
	ClassConcurrencyLimit Class = "concurrency_limit"
	ClassRateLimit        Class = "rate_limit"
	ClassStreamDisconnect Class = "stream_disconnect"
	ClassAuth             Class = "auth"
	ClassQuota            Class = "quota"
	ClassServerError      Class = "server_error"
)

// ClassOf maps a failed run to an incident class. ok is false for failures that
// are not provider incidents (a bad prompt, a crash in one repo). Text wins over
// failure_reason: "Concurrency limit exceeded for account" classifies as
// agent_error.unknown, which is why the provider breaker never saw the 25/11 outage.
func ClassOf(failureReason, errText string) (Class, bool) {
	if strings.Contains(strings.ToLower(errText), "concurrency limit exceeded") {
		return ClassConcurrencyLimit, true
	}
	switch taskfailure.Reason(failureReason) {
	case taskfailure.ReasonAgentProviderCapacityOrRateLimit:
		return ClassRateLimit, true
	case taskfailure.ReasonAgentProviderNetwork:
		return ClassStreamDisconnect, true
	case taskfailure.ReasonAgentProviderAuthOrAccess:
		return ClassAuth, true
	case taskfailure.ReasonAgentProviderQuotaLimit:
		return ClassQuota, true
	case taskfailure.ReasonAgentProviderServerError:
		return ClassServerError, true
	}
	return "", false
}
