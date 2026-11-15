package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type listedPasskeys struct {
	Passkeys []struct {
		ID         string   `json:"id"`
		CreatedAt  string   `json:"created_at"`
		LastUsedAt *string  `json:"last_used_at"`
		Transports []string `json:"transports"`
		State      string   `json:"state"`
	} `json:"passkeys"`
}

func listRequest(user, source string) *http.Request {
	request := newRequest(http.MethodGet, "/api/passkeys", nil)
	request.Header.Set("X-User-ID", user)
	request.Header.Set("X-Actor-Source", source)
	return request
}

func TestListPasskeysReturnsOnlyTheCallersUsableKeys(test *testing.T) {
	pair := newTwoPasskeys(test)
	strangerPasskey(test)
	revoked := passkeytest.New(test)
	var revokedID string
	dbfx.QueryRow(test, `INSERT INTO user_passkey (user_id, credential_id, public_key, sign_count, flags, revoked_at) VALUES ($1, $2, $3, $4, 5, now()) RETURNING id`,
		pair.humanID, revoked.CredentialID, revoked.COSEPublicKey(test), passkeyStart).Scan(&revokedID)

	var listed listedPasskeys
	decodeJSON(test, testutil.Call(test, pair.handler.ListPasskeys, listRequest(pair.humanID, "interactive_jwt")).Want(http.StatusOK).Text(), &listed)

	ids := map[string]bool{}
	for _, item := range listed.Passkeys {
		ids[item.ID] = true
		if item.CreatedAt == "" {
			test.Errorf("passkey %s has no created_at", item.ID)
		}
	}
	if len(ids) != 2 || !ids[pair.firstID] || !ids[pair.secondID] {
		test.Fatalf("listed %v, want exactly %s and %s", ids, pair.firstID, pair.secondID)
	}
}

func TestListPasskeysShowsSuspectKeysWithTheirState(test *testing.T) {
	pair := newTwoPasskeys(test)
	dbfx.Exec(test, `UPDATE user_passkey SET suspect_at = now() WHERE id = $1`, pair.secondID)

	var listed listedPasskeys
	decodeJSON(test, testutil.Call(test, pair.handler.ListPasskeys, listRequest(pair.humanID, "interactive_jwt")).Want(http.StatusOK).Text(), &listed)

	states := map[string]string{}
	for _, item := range listed.Passkeys {
		states[item.ID] = item.State
	}
	if len(states) != 2 || states[pair.firstID] != "active" || states[pair.secondID] != "suspect" {
		test.Fatalf("states %v, want %s active and %s suspect", states, pair.firstID, pair.secondID)
	}
}

func TestListPasskeysNeverLeaksKeyMaterial(test *testing.T) {
	pair := newTwoPasskeys(test)
	body := testutil.Call(test, pair.handler.ListPasskeys, listRequest(pair.humanID, "interactive_jwt")).Want(http.StatusOK).Text()
	for _, secret := range []string{"credential_id", "public_key", "sign_count", "user_id"} {
		if strings.Contains(body, secret) {
			test.Errorf("response exposes %s: %s", secret, body)
		}
	}
}

func TestListPasskeysReturnsAnEmptyArrayWhenThereAreNone(test *testing.T) {
	handler := passkeyHandler(test)
	user := enrollUser(test)
	body := testutil.Call(test, handler.ListPasskeys, listRequest(user, "interactive_jwt")).Want(http.StatusOK).Text()
	if !strings.Contains(body, `"passkeys":[]`) {
		test.Fatalf("body %s, want an empty passkeys array", body)
	}
}

func TestListPasskeysRefusesNonInteractiveCallers(test *testing.T) {
	pair := newTwoPasskeys(test)
	for _, source := range []string{"pat", "cli_jwt", "task_token", ""} {
		testutil.Call(test, pair.handler.ListPasskeys, listRequest(pair.humanID, source)).Want(http.StatusForbidden)
	}
}
