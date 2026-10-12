package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
)

func TestAuthJWTProvenance(test *testing.T) {
	for _, testcase := range []struct {
		name string
		amr  any
		want string
	}{
		{name: "interactive", amr: "interactive", want: "interactive_jwt"},
		{name: "cli", amr: "cli", want: "cli_jwt"},
		{name: "legacy", want: "cli_jwt"},
		{name: "unknown", amr: "other", want: "cli_jwt"},
		{name: "array", amr: []string{"interactive"}, want: "cli_jwt"},
	} {
		for _, useCookie := range []bool{false, true} {
			test.Run(testcase.name+map[bool]string{false: "/bearer", true: "/cookie"}[useCookie], func(test *testing.T) {
				claims := validClaims()
				if testcase.amr != nil {
					claims["amr"] = testcase.amr
				}
				token := generateToken(claims, auth.JWTSecret())
				req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
				if useCookie {
					req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: token})
				} else {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				req.Header.Set("X-Actor-Source", "interactive_jwt")
				handler := authMiddleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if got := request.Header.Get("X-Actor-Source"); got != testcase.want {
						test.Errorf("auth path = %q, want %q", got, testcase.want)
					}
					writer.WriteHeader(http.StatusOK)
				}))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if response.Code != http.StatusOK {
					test.Fatalf("status = %d: %s", response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestAuthPATCacheProvenance(test *testing.T) {
	cache := auth.NewPATCache(newRedisTestClient(test))
	const rawToken = "mul_provenance_cache_test"
	cache.Set(context.Background(), auth.HashToken(rawToken), "cached-user-id", auth.AuthCacheTTL)
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	response := httptest.NewRecorder()
	Auth(nil, cache, nil)(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("X-Actor-Source"); got != "pat" {
			test.Errorf("cached PAT auth path = %q, want pat", got)
		}
		writer.WriteHeader(http.StatusOK)
	})).ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		test.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}
