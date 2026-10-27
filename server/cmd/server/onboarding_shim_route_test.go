package main

import (
	"net/http"
	"testing"
)

// Desktop builds before v3 call these two routes during onboarding. The
// handlers stay until X-Client-Version telemetry shows no such install is
// still active; an unauthenticated call must reach the auth gate, not a 404.
func TestPreV3OnboardingShimRoutesStayRegistered(t *testing.T) {
	for _, path := range []string{
		"/api/me/onboarding/runtime-bootstrap",
		"/api/me/onboarding/no-runtime-bootstrap",
	} {
		t.Run(path, func(t *testing.T) {
			response, err := http.Post(testServer.URL+path, "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("POST %s = %d, want 401: the route is not registered", path, response.StatusCode)
			}
		})
	}
}
