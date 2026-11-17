package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// specFixture is a temp tree standing in for one run's host: a HOME with the
// never-mounted directories, a toolchain, and a cached repository.
type specFixture struct {
	root, home string
	in         runSandboxInput
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func newSpecFixture(t *testing.T) specFixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "userhome")
	t.Setenv("HOME", home) // DefaultDeny reads the user's home
	for _, d := range []string{".ssh", ".config/gh", ".config/github-apps", ".claude", ".codex"} {
		mkdirs(t, filepath.Join(home, d))
	}
	bin := filepath.Join(root, "tools", "bin")
	mkdirs(t, bin, filepath.Join(root, "run", "work"), filepath.Join(root, "run", "tmp"), filepath.Join(root, "run", "home"),
		filepath.Join(root, "guard"), filepath.Join(root, "brk", "bin"), filepath.Join(root, "brk", "s"),
		filepath.Join(root, "cache", "r1.git", "objects"))
	agentExe := filepath.Join(bin, "claude")
	daemonExe := filepath.Join(root, "daemon", "multica")
	mkdirs(t, filepath.Dir(daemonExe))
	for _, f := range []string{agentExe, daemonExe} {
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return specFixture{root: root, home: home, in: runSandboxInput{
		WorkDir: filepath.Join(root, "run", "work"), TempDir: filepath.Join(root, "run", "tmp"), Home: filepath.Join(root, "run", "home"),
		Env:      map[string]string{"PATH": bin, "FOO": "bar"},
		AgentExe: agentExe, GuardShimDir: filepath.Join(root, "guard"), DaemonExe: daemonExe,
		Repos: []runSandboxRepo{{URL: "https://github.com/o/r1", Bare: filepath.Join(root, "cache", "r1.git")}},
	}}
}

func TestBuildRunSandboxSpec_Table(t *testing.T) {
	for _, tc := range []struct {
		name           string
		broker, egress bool
	}{
		{"no broker no egress", false, false},
		{"broker only", true, false},
		{"egress only", false, true},
		{"broker and egress", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSpecFixture(t)
			in := f.in
			var wantBroker, wantEgress string
			if tc.broker {
				in.BrokerSocket, in.BrokerShimDir = filepath.Join(f.root, "brk", "s", "b.sock"), filepath.Join(f.root, "brk", "bin")
				wantBroker = in.BrokerSocket
			}
			if tc.egress {
				in.EgressSocket = filepath.Join(f.root, "egress.sock")
				wantEgress = in.EgressSocket
			}
			got, notes := buildRunSandboxSpec(in)
			wantRO := []string{
				filepath.Join(f.root, "tools", "bin"), // agent exe dir (no package.json above) and the PATH dir, deduped
				in.GuardShimDir,
			}
			if tc.broker {
				wantRO = append(wantRO, in.BrokerShimDir)
			}
			wantRO = append(wantRO, filepath.Dir(in.DaemonExe), repocache.ObjectsDir(in.Repos[0].Bare))
			want := sandbox.Spec{
				WorkDir: in.WorkDir, TempDir: in.TempDir, Home: in.Home,
				ReadOnly: wantRO, Env: []string{"FOO=bar", "PATH=" + in.Env["PATH"]},
				BrokerSocket: wantBroker, EgressSocket: wantEgress,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("spec mismatch\n got: %#v\nwant: %#v", got, want)
			}
			if len(notes) != 0 {
				t.Fatalf("unexpected notes: %v", notes)
			}
		})
	}
}

func TestBuildRunSandboxSpec_NeverMountedPathsDropped(t *testing.T) {
	f := newSpecFixture(t)
	in := f.in
	denied := []string{
		filepath.Join(f.home, ".ssh"), filepath.Join(f.home, ".config", "gh"), filepath.Join(f.home, ".config", "github-apps"),
		filepath.Join(f.home, ".claude"), filepath.Join(f.home, ".codex"),
	}
	// On PATH, extra, and as the agent executable root: none may become a bind.
	in.Env = map[string]string{"PATH": strings.Join(append([]string{in.Env["PATH"]}, denied...), ":")}
	in.ExtraReadOnly = []string{filepath.Join(f.home, ".ssh", "keys"), "/var/run/docker.sock"}
	exe := filepath.Join(f.home, ".claude", "local", "claude")
	mkdirs(t, filepath.Dir(exe))
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	in.AgentExe = exe
	got, _ := buildRunSandboxSpec(in)
	for _, ro := range got.ReadOnly {
		for _, d := range append(sandbox.DefaultDeny(), f.home) {
			if ro == d || strings.HasPrefix(ro, d+"/") {
				t.Errorf("ReadOnly %q overlaps never-mounted %q", ro, d)
			}
		}
	}
	// A symlink into a never-mounted dir is also dropped.
	link := filepath.Join(f.root, "sneaky")
	if err := os.Symlink(filepath.Join(f.home, ".ssh"), link); err != nil {
		t.Fatal(err)
	}
	in.ExtraReadOnly = []string{link}
	got, _ = buildRunSandboxSpec(in)
	for _, ro := range got.ReadOnly {
		if ro == link {
			t.Errorf("symlink to ~/.ssh became a bind")
		}
	}
}

func TestBuildRunSandboxSpec_InstallRoot(t *testing.T) {
	f := newSpecFixture(t)
	pkg := filepath.Join(f.root, "lib", "node_modules", "claude-code")
	mkdirs(t, filepath.Join(pkg, "dist"), filepath.Join(f.root, "lbin"))
	for _, p := range []string{filepath.Join(pkg, "package.json"), filepath.Join(pkg, "dist", "cli.js")} {
		if err := os.WriteFile(p, []byte("{}"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(f.root, "lbin", "claude")
	if err := os.Symlink(filepath.Join(pkg, "dist", "cli.js"), link); err != nil {
		t.Fatal(err)
	}
	in := f.in
	in.AgentExe = link
	got, _ := buildRunSandboxSpec(in)
	if !containsPath(got.ReadOnly, pkg) {
		t.Fatalf("install root %s missing from %v", pkg, got.ReadOnly)
	}
}

func containsPath(ps []string, p string) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

func TestBuildRunSandboxSpec_ExtraReadOnlyAndMissingPath(t *testing.T) {
	f := newSpecFixture(t)
	in := f.in
	scripts := filepath.Join(f.root, "scripts")
	mkdirs(t, scripts)
	in.ExtraReadOnly = []string{scripts}
	in.Env["PATH"] += ":" + filepath.Join(f.root, "nope")
	got, _ := buildRunSandboxSpec(in)
	if !containsPath(got.ReadOnly, scripts) {
		t.Errorf("extra read-only %s missing from %v", scripts, got.ReadOnly)
	}
	if containsPath(got.ReadOnly, filepath.Join(f.root, "nope")) {
		t.Errorf("non-existent PATH dir became a bind")
	}
}

func TestBuildRunSandboxSpec_UncachedRepoGetsNoBindAndIsReported(t *testing.T) {
	f := newSpecFixture(t)
	in := f.in
	in.Repos = append(in.Repos, runSandboxRepo{URL: "https://github.com/o/uncached"})
	got, notes := buildRunSandboxSpec(in)
	if len(got.ReadOnly) == 0 || !containsPath(got.ReadOnly, repocache.ObjectsDir(in.Repos[0].Bare)) {
		t.Fatalf("cached repo objects dir not bound: %v", got.ReadOnly)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "https://github.com/o/uncached") {
		t.Fatalf("notes = %v, want one naming the uncached repo", notes)
	}
}
