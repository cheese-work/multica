package mint

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

func noStore(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
}

// WriteToken writes a 200 token response with Cache-Control: no-store (rule 9).
func WriteToken(w http.ResponseWriter, tok Token) {
	noStore(w)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		Token       string      `json:"token"`
		ExpiresAt   time.Time   `json:"expires_at"`
		Permissions Permissions `json:"permissions"`
	}{tok.Value.Reveal(), tok.ExpiresAt, tok.Permissions})
}

// WriteRefusal writes a refusal response with Cache-Control: no-store and, for
// a retryable refusal, Retry-After in whole seconds.
func WriteRefusal(w http.ResponseWriter, ref *Refusal) {
	noStore(w)
	if ref.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((ref.RetryAfter+time.Second-1)/time.Second)))
	}
	w.WriteHeader(ref.Status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": ref.Reason})
}
