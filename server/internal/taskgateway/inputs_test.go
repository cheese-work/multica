package taskgateway

import (
	"bytes"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func TestTaskGatewayInputSnapshot(test *testing.T) {
	snapshot := InputSnapshot{Binding: credentialexec.Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"}, RuntimeID: "00000000-0000-4000-8000-000000000004", AgentID: "00000000-0000-4000-8000-000000000005", DispatchedAt: "2026-10-09T17:00:00Z", Instructions: "authorized instructions", WorkspaceContext: "authorized context"}
	contents, err := EncodeInputSnapshot(snapshot)
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeInputSnapshot(contents)
	if err != nil || decoded != snapshot {
		test.Fatal("authenticated snapshot did not round-trip", err)
	}
	for _, invalid := range [][]byte{
		append(append([]byte(nil), contents...), []byte(`{}`)...),
		bytes.Replace(contents, []byte(`"instructions":"authorized instructions"`), []byte(`"instructions":null`), 1),
		bytes.Replace(contents, []byte(`"instructions":"authorized instructions"`), []byte(`"instructions":"first","instructions":"last"`), 1),
		bytes.Replace(contents, []byte(`"workspace_context":"authorized context"`), []byte(`"unknown":"value"`), 1),
		bytes.Replace(contents, []byte(snapshot.RuntimeID), []byte("forged-runtime"), 1),
		bytes.Replace(contents, []byte(snapshot.DispatchedAt), []byte("unknown"), 1),
		bytes.Replace(contents, []byte(snapshot.Instructions), []byte(strings.Repeat("X", (1<<20)+1)), 1),
		bytes.Replace(contents, []byte(snapshot.Instructions), []byte{0xff}, 1),
	} {
		if _, err := DecodeInputSnapshot(invalid); err == nil {
			test.Fatal("unsafe input snapshot accepted")
		}
	}
}
