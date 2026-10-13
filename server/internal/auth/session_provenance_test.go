package auth

import (
	"reflect"
	"testing"
	"time"
)

func TestRenewSessionTokenPreservesAuthProvenance(test *testing.T) {
	methods := map[string]any{
		"interactive": "interactive",
		"cli":         "cli",
		"legacy":      nil,
		"unknown":     "other",
		"array":       []any{"interactive"},
	}
	for name, method := range methods {
		test.Run(name, func(test *testing.T) {
			claims := sessionClaims(test, time.Now().Add(time.Hour), "session-provenance")
			if method != nil {
				claims["amr"] = method
			}
			renewedToken, _, err := RenewSessionToken(claims)
			if err != nil {
				test.Fatal(err)
			}
			renewed, err := ParseSessionToken(renewedToken)
			if err != nil {
				test.Fatal(err)
			}
			if !reflect.DeepEqual(renewed["amr"], method) {
				test.Fatalf("renewed amr = %#v, want %#v", renewed["amr"], method)
			}
			if _, present := renewed["amr"]; present != (method != nil) {
				test.Fatal("renewal changed auth provenance claim presence")
			}
		})
	}
}
