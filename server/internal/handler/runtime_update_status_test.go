package handler

import (
	"encoding/json"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	serverBuildCommit = "0123456789abcdef0123456789abcdef01234567"
	olderBuildCommit  = "89abcdef0123456789abcdef0123456789abcdef"
)

func testUpdateSource(commit string) *DaemonUpdateSource {
	return &DaemonUpdateSource{manifest: DaemonUpdateManifest{
		Commit:  commit,
		Version: "0.7.1-20261029-2357",
		Binaries: map[string]DaemonUpdateBinary{
			"linux-amd64":  {SHA256: strings.Repeat("a", 64), Size: 1},
			"darwin-arm64": {SHA256: strings.Repeat("b", 64), Size: 1},
		},
	}}
}

func runtimeMeta(commit, goos, goarch string) []byte {
	m := map[string]any{"cli_version": "0.7.1"}
	if commit != "" {
		m["cli_commit"] = commit
	}
	if goos != "" {
		m["cli_os"] = goos
	}
	if goarch != "" {
		m["cli_arch"] = goarch
	}
	b, _ := json.Marshal(m)
	return b
}

func TestRuntimeUpdateStatus(t *testing.T) {
	tests := []struct {
		name string
		src  *DaemonUpdateSource
		meta []byte
		want RuntimeUpdateStatus
	}{
		{
			name: "commits differ",
			src:  testUpdateSource(serverBuildCommit),
			meta: runtimeMeta(olderBuildCommit, "linux", "amd64"),
			want: RuntimeUpdateStatus{UpdateAvailable: true, ServerCommit: serverBuildCommit, ServerVersion: "0.7.1-20261029-2357"},
		},
		{
			name: "commits differ on darwin",
			src:  testUpdateSource(serverBuildCommit),
			meta: runtimeMeta(olderBuildCommit, "darwin", "arm64"),
			want: RuntimeUpdateStatus{UpdateAvailable: true, ServerCommit: serverBuildCommit, ServerVersion: "0.7.1-20261029-2357"},
		},
		{
			name: "commits equal",
			src:  testUpdateSource(serverBuildCommit),
			meta: runtimeMeta(serverBuildCommit, "linux", "amd64"),
			want: RuntimeUpdateStatus{ServerCommit: serverBuildCommit, ServerVersion: "0.7.1-20261029-2357"},
		},
		{
			name: "abbreviated runtime commit that prefixes the server commit is equal",
			src:  testUpdateSource(serverBuildCommit),
			meta: runtimeMeta(serverBuildCommit[:10], "linux", "amd64"),
			want: RuntimeUpdateStatus{ServerCommit: serverBuildCommit, ServerVersion: "0.7.1-20261029-2357"},
		},
		{
			name: "runtime commit is the ldflags default",
			src:  testUpdateSource(serverBuildCommit),
			meta: runtimeMeta("unknown", "linux", "amd64"),
			want: RuntimeUpdateStatus{ServerCommit: serverBuildCommit, ServerVersion: "0.7.1-20261029-2357", Reason: "unknown_commit"},
		},
		{
			name: "daemon predates cli_commit",
			src:  testUpdateSource(serverBuildCommit),
			meta: runtimeMeta("", "", ""),
			want: RuntimeUpdateStatus{ServerCommit: serverBuildCommit, ServerVersion: "0.7.1-20261029-2357", Reason: "unknown_commit"},
		},
		{
			name: "runtime commit is not hex",
			src:  testUpdateSource(serverBuildCommit),
			meta: runtimeMeta("v0.7.1-dirty", "linux", "amd64"),
			want: RuntimeUpdateStatus{ServerCommit: serverBuildCommit, ServerVersion: "0.7.1-20261029-2357", Reason: "unknown_commit"},
		},
		{
			name: "no manifest on this server",
			src:  nil,
			meta: runtimeMeta(olderBuildCommit, "linux", "amd64"),
			want: RuntimeUpdateStatus{Reason: "no_build"},
		},
		{
			name: "windows runtime",
			src:  testUpdateSource(serverBuildCommit),
			meta: runtimeMeta(olderBuildCommit, "windows", "amd64"),
			want: RuntimeUpdateStatus{ServerCommit: serverBuildCommit, ServerVersion: "0.7.1-20261029-2357", Reason: "unsupported_os"},
		},
		{
			name: "manifest has no build for the platform",
			src:  testUpdateSource(serverBuildCommit),
			meta: runtimeMeta(olderBuildCommit, "linux", "arm64"),
			want: RuntimeUpdateStatus{ServerCommit: serverBuildCommit, ServerVersion: "0.7.1-20261029-2357", Reason: "no_build"},
		},
		{
			name: "empty metadata",
			src:  testUpdateSource(serverBuildCommit),
			meta: nil,
			want: RuntimeUpdateStatus{ServerCommit: serverBuildCommit, ServerVersion: "0.7.1-20261029-2357", Reason: "unknown_commit"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.src.runtimeUpdateStatus(tt.meta); got != tt.want {
				t.Fatalf("runtimeUpdateStatus = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRuntimeResponseCarriesUpdateStatus(t *testing.T) {
	h := &Handler{DaemonUpdate: testUpdateSource(serverBuildCommit)}
	rt := db.AgentRuntime{Metadata: runtimeMeta(olderBuildCommit, "linux", "amd64")}

	raw, err := json.Marshal(h.runtimeResponse(rt))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["update_available"] != true || got["server_commit"] != serverBuildCommit ||
		got["server_version"] != "0.7.1-20261029-2357" {
		t.Fatalf("response = %s", raw)
	}
	if _, ok := got["update_reason"]; ok {
		t.Fatalf("update_reason must be omitted when an update is available: %s", raw)
	}

	bare := &Handler{}
	raw, _ = json.Marshal(bare.runtimeResponse(rt))
	got = map[string]any{}
	_ = json.Unmarshal(raw, &got)
	if got["update_available"] != false || got["update_reason"] != "no_build" {
		t.Fatalf("server without a build: %s", raw)
	}
}
