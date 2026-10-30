package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNotificationPreferenceDeliveryScopeValidation(test *testing.T) {
	for _, scenario := range []struct {
		name, body string
		valid      bool
	}{
		{"restrict other activity", `{"preferences":{"unassigned_activity":"muted","comments":"muted"}}`, true},
		{"restore all activity", `{"preferences":{"unassigned_activity":"all"}}`, true},
		{"reject unsupported scope value", `{"preferences":{"unassigned_activity":"decisions_only"}}`, false},
		{"reject unknown preference", `{"preferences":{"unknown":"muted"}}`, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPatch, "/api/notification-preferences", strings.NewReader(scenario.body))
			_, valid := decodeNotificationPreferenceRequest(response, request)
			if valid != scenario.valid {
				test.Fatalf("valid=%v, want %v; response=%s", valid, scenario.valid, response.Body.String())
			}
		})
	}
}
