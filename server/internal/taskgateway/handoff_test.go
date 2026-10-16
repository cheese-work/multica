package taskgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func TestTaskGatewayStrictHandoff(test *testing.T) {
	_, binding, _ := gatewayFixture(test)
	contents := `{"binding":{"task_id":"` + binding.TaskID + `","owner_id":"` + binding.OwnerID + `","workspace_id":"` + binding.WorkspaceID + `"},"base_url":"https://owned.example","key":"owned-gateway-secret"}`
	grant, err := DecodeHandoff([]byte(contents), binding)
	if err != nil || grant.Binding != binding || grant.Key != "owned-gateway-secret" {
		test.Fatal("trusted handoff refused")
	}
	serialized, err := json.Marshal(grant)
	if err != nil || strings.Contains(string(serialized)+fmt.Sprintf("%#v", grant), "owned-gateway-secret") {
		test.Fatal("ordinary grant serialization exposed secret")
	}
	for _, invalid := range []string{
		`null`, contents + `{}`, strings.Replace(contents, `"key":`, `"key":"forged","KEY":`, 1),
		strings.Replace(contents, `"key":`, `"unknown":true,"key":`, 1),
		strings.Replace(contents, binding.OwnerID, binding.TaskID, 1),
		strings.Replace(contents, binding.TaskID, binding.OwnerID, 1),
		strings.Replace(contents, binding.WorkspaceID, binding.TaskID, 1),
		strings.Replace(contents, `owned-gateway-secret`, `bad\r\nheader`, 1),
		strings.Replace(contents, `owned-gateway-secret`, ``, 1),
		strings.Replace(contents, `https://owned.example`, `http://owned.example`, 1),
		strings.Replace(contents, `https://owned.example`, `https://user:secret@owned.example`, 1),
		strings.Replace(contents, `https://owned.example`, `https://owned.example?secret=owned-gateway-secret`, 1),
		strings.Repeat(" ", (1<<20)+1),
	} {
		if _, err := DecodeHandoff([]byte(invalid), binding); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "owned-gateway-secret") {
			test.Fatal("unsafe handoff admitted or secret echoed")
		}
	}
}

func TestTaskGatewayHandoffPreparesBeforeFetching(test *testing.T) {
	if _, err := os.Stat("/usr/bin/bwrap"); os.IsNotExist(err) {
		test.Skip("handoff preparation namespace integration NOT-RUN: system bubblewrap unavailable")
	}
	_, binding, _ := gatewayFixture(test)
	spec := credentialexec.Spec{Root: filepath.Join(test.TempDir(), "private"), Binding: binding, Provider: "codex", Executable: "/owned/missing-elf", HelperExecutable: "/owned/missing-helper"}
	fetched := false
	boundary, err := PrepareHandoff(context.Background(), spec, func(context.Context) (Grant, error) { fetched = true; return Grant{}, ErrUnavailable })
	if boundary != nil || err == nil || fetched {
		test.Fatal("failed OS preparation fetched a credential")
	}
	helper, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	contents, err := os.ReadFile(helper)
	if err != nil {
		test.Fatal(err)
	}
	executable := filepath.Join(test.TempDir(), "owned-elf")
	if err := os.WriteFile(executable, contents, 0500); err != nil {
		test.Fatal(err)
	}
	spec.Executable, spec.HelperExecutable = executable, helper
	spec.Root = filepath.Join(test.TempDir(), "prepared-private")
	boundary, err = PrepareHandoff(context.Background(), spec, func(context.Context) (Grant, error) {
		fetched = true
		if _, err := os.Stat(filepath.Join(spec.Root, binding.TaskID, "binding.json")); err != nil {
			test.Fatal("credential requested before boundary exists")
		}
		return Grant{}, ErrUnavailable
	})
	if boundary != nil || err == nil || !fetched {
		test.Fatal("refused handoff did not close prepared boundary")
	}
}
