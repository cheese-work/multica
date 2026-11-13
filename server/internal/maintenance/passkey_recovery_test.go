package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func recoverRequest(handler http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/maintenance/passkey-recovery", bytes.NewBufferString(body)))
	return rec
}

func TestPasskeyRecoveryRejectsMalformedRequestsWithoutDatabase(t *testing.T) {
	h := NewHandler(NewService(nil, StatusCategory{}))
	for _, body := range []string{"null", "[]", "{}", `{"email":""}`, `{"email":"not an email"}`, `{"email":"a@b.test","extra":1}`, `{"email":"a@b.test"} {}`} {
		if rec := recoverRequest(h, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: status=%d body=%s, want 400", body, rec.Code, rec.Body.String())
		}
	}
}

// The recovery runs against the real schema: it needs the user, passkey and
// audit tables that the job tests' private schema does not have.
func TestPasskeyRecoveryEndpointRecoversTheUser(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL required; run through make env-exec")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	email := fmt.Sprintf("recover-http-%d@example.test", time.Now().UnixNano())
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Recovery http', $1) RETURNING id::text`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM passkey_event WHERE user_id = $1`, id)
		pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, id)
	})
	h := NewHandler(NewService(pool, StatusCategory{}))

	rec := recoverRequest(h, fmt.Sprintf(`{"email":%q}`, email))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Recovery struct {
			UserID     string `json:"user_id"`
			Generation int    `json:"session_generation"`
			Revoked    int    `json:"revoked_passkeys"`
			State      string `json:"passkey_state"`
		} `json:"passkey_recovery"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Recovery.UserID != id || out.Recovery.Generation != 1 || out.Recovery.State != "recovery_required" {
		t.Fatalf("response %+v", out.Recovery)
	}
	if rec := recoverRequest(h, `{"email":"nobody-`+email+`"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown email: status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
}
