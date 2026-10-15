package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/multica-ai/multica/server/internal/auth"
)

func TestDesktopTokenRoute(test *testing.T) {
	for _, method := range []string{"interactive", "cli", "", "unknown"} {
		test.Run("bearer/"+method, func(test *testing.T) {
			claims := jwt.MapClaims{"sub": testUserID, "sid": "original-session", "exp": time.Now().Add(time.Hour).Unix()}
			if method != "" {
				claims["amr"] = method
			}
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.JWTSecret())
			if err != nil {
				test.Fatal(err)
			}
			request, err := http.NewRequest(http.MethodPost, testServer.URL+"/api/auth/desktop-token", nil)
			if err != nil {
				test.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				test.Fatal(err)
			}
			defer response.Body.Close()
			want := http.StatusForbidden
			if method == "interactive" {
				want = http.StatusOK
			}
			if response.StatusCode != want {
				test.Fatalf("desktop-token status = %d, want %d", response.StatusCode, want)
			}
			if method == "interactive" {
				var result map[string]string
				readJSON(test, response, &result)
				issued, err := jwt.Parse(result["token"], func(token *jwt.Token) (any, error) { return auth.JWTSecret(), nil }, jwt.WithValidMethods([]string{"HS256"}))
				if err != nil {
					test.Fatal(err)
				}
				issuedClaims := issued.Claims.(jwt.MapClaims)
				if issuedClaims["amr"] != "interactive" || issuedClaims["sid"] == nil || issuedClaims["sid"] == "original-session" || issuedClaims["sid"] == "" {
					test.Fatalf("desktop-token claims = %v", issuedClaims)
				}
			}
		})
	}
}
