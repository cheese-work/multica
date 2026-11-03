package middleware

import "net/http"

func writeAuthenticationError(w http.ResponseWriter, r *http.Request, message string) {
	http.Error(w, message, http.StatusUnauthorized)
}

func writeInvalidTaskToken(w http.ResponseWriter, r *http.Request) {
	http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
}
