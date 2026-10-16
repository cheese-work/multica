package taskgateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/governance/credential"
)

func taskGatewayConfigFixture(test *testing.T) ([]byte, *credential.Keyring, string) {
	test.Helper()
	material := bytes.Repeat([]byte{7}, 32)
	ring, err := credential.NewKeyring(credential.Key{ID: credential.KeyID(material), Material: material})
	if err != nil {
		test.Fatal(err)
	}
	envelope, err := ring.Seal([]byte("owned-operator-secret"), operatorCredentialBinding())
	if err != nil {
		test.Fatal(err)
	}
	contents, err := json.Marshal(map[string]any{
		"base_url": "https://gateway.example", "operator_credential": envelope,
		"policies": []Policy{{RuntimeID: "00000000-0000-4000-8000-000000000004", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003", TaskID: "00000000-0000-4000-8000-000000000001", GatewayOwnerID: 7, Limit: 100, KeyIDs: map[string]int64{"claude": 11, "codex": 12}}},
	})
	if err != nil {
		test.Fatal(err)
	}
	return contents, ring, base64.StdEncoding.EncodeToString(material)
}

func TestTaskGatewayEncryptedConfiguration(test *testing.T) {
	contents, ring, _ := taskGatewayConfigFixture(test)
	operator, err := DecodeConfiguration(contents, ring)
	if err != nil || !operator.Requires("00000000-0000-4000-8000-000000000004") {
		test.Fatalf("owned encrypted config refused: %v", err)
	}
	if strings.Contains(string(contents), "owned-operator-secret") || strings.Contains(fmt.Sprintf("%#v", operator), "owned-operator-secret") {
		test.Fatal("configuration exposed operator secret")
	}
	for _, invalid := range [][]byte{
		[]byte(`null`), append(append([]byte{}, contents...), []byte(`{}`)...),
		bytes.Replace(contents, []byte(`"base_url":`), []byte(`"base_url":"https://forged.example","BASE_URL":`), 1),
		bytes.Replace(contents, []byte(`"base_url":`), []byte(`"unknown":true,"base_url":`), 1),
		bytes.Replace(contents, []byte(`"operator_credential":{`), []byte(`"operator_credential":{"token":"owned-operator-secret",`), 1),
		bytes.Replace(contents, []byte(`"operator_credential":{`), []byte(`"operator_credential":{"nonce":"forged","Nonce":"forged",`), 1),
		bytes.Replace(contents, []byte(`"limit_tenths":100`), []byte(`"limit_tenths":0`), 1),
		bytes.Repeat([]byte(" "), (1<<20)+1),
	} {
		if _, err := DecodeConfiguration(invalid, ring); err == nil || strings.Contains(err.Error(), "owned-operator-secret") {
			test.Fatal("malformed trusted config admitted or secret in error")
		}
	}
	wrong, err := credential.NewKeyring(credential.Key{ID: "wrong", Material: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := DecodeConfiguration(contents, wrong); err == nil {
		test.Fatal("wrong encryption key admitted")
	}
	if _, err := DecodeConfiguration(contents, nil); err == nil {
		test.Fatal("missing keyring admitted")
	}
	envelope, err := ring.Seal([]byte("owned-operator-secret"), credential.Binding{WorkspaceID: "other", CredentialID: "task-gateway", Purpose: "operator-api"})
	if err != nil {
		test.Fatal(err)
	}
	var document map[string]any
	if json.Unmarshal(contents, &document) != nil {
		test.Fatal("fixture decode")
	}
	document["operator_credential"] = envelope
	changed, err := json.Marshal(document)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := DecodeConfiguration(changed, ring); err == nil {
		test.Fatal("cross-purpose envelope admitted")
	}
}

func TestTaskGatewayConfigurationDefaultOffAndRefusal(test *testing.T) {
	contents, _, key := taskGatewayConfigFixture(test)
	test.Setenv("MULTICA_TASK_GATEWAY_CONFIG", "")
	test.Setenv("MULTICA_TASK_GATEWAY_SECRET_KEY", "")
	if operator, err := LoadFromEnv(); operator != nil || err != nil {
		test.Fatal("default-off failed")
	}
	test.Setenv("MULTICA_TASK_GATEWAY_SECRET_KEY", key)
	if _, err := LoadFromEnv(); err == nil {
		test.Fatal("partial configuration silently disabled policy")
	}
	path := filepath.Join(test.TempDir(), "trusted.json")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		test.Fatal(err)
	}
	test.Setenv("MULTICA_TASK_GATEWAY_CONFIG", path)
	if operator, err := LoadFromEnv(); operator == nil || err != nil {
		test.Fatal("owned trusted config refused")
	}
	if err := os.Chmod(path, 0666); err != nil {
		test.Fatal(err)
	}
	if _, err := LoadFromEnv(); err == nil {
		test.Fatal("untrusted writable file admitted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		test.Fatal(err)
	}
	link := filepath.Join(test.TempDir(), "link.json")
	if err := os.Symlink(path, link); err != nil {
		test.Fatal(err)
	}
	test.Setenv("MULTICA_TASK_GATEWAY_CONFIG", link)
	if _, err := LoadFromEnv(); err == nil {
		test.Fatal("symlink configuration admitted")
	}
	test.Setenv("MULTICA_TASK_GATEWAY_CONFIG", "relative.json")
	if _, err := LoadFromEnv(); err == nil {
		test.Fatal("task-relative configuration admitted")
	}
	test.Setenv("MULTICA_TASK_GATEWAY_CONFIG", path)
	test.Setenv("MULTICA_TASK_GATEWAY_SECRET_KEY", "invalid-owned-key")
	if _, err := LoadFromEnv(); err == nil {
		test.Fatal("invalid key silently disabled policy")
	}
	test.Setenv("MULTICA_TASK_GATEWAY_SECRET_KEY", key)
	test.Setenv("MULTICA_TASK_GATEWAY_CONFIG", filepath.Join(test.TempDir(), "absent"))
	if _, err := LoadFromEnv(); err == nil {
		test.Fatal("unavailable configuration silently disabled policy")
	}
}
