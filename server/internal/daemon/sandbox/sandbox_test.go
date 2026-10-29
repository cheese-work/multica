package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newLauncher() *Launcher {
	return &Launcher{Mode: ModeOn, Logger: quiet(), Deny: DefaultDeny()}
}

// layout is one fake task tree: a sibling task, a repo cache with metadata,
// and the three writable dirs of the task under test.
type layout struct {
	root, work, tmp, home, otherWork, cache, cacheObjects string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	root := t.TempDir()
	l := layout{
		root:         root,
		work:         filepath.Join(root, "task-a", "workdir"),
		tmp:          filepath.Join(root, "task-a", "tmp"),
		home:         filepath.Join(root, "task-a", "home"),
		otherWork:    filepath.Join(root, "task-b", "workdir"),
		cache:        filepath.Join(root, "repos", "r.git"),
		cacheObjects: filepath.Join(root, "repos", "r.git", "objects"),
	}
	for _, d := range []string{l.work, l.tmp, l.home, l.otherWork, l.cacheObjects, filepath.Join(l.cache, "worktrees")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(l.otherWork, "secret"), "other-task")
	mustWrite(t, filepath.Join(l.cache, "config"), "cache-meta")
	mustWrite(t, filepath.Join(l.cacheObjects, "obj"), "object")
	return l
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (l layout) spec(script string) Spec {
	return Spec{
		WorkDir:  l.work,
		TempDir:  l.tmp,
		Home:     l.home,
		ReadOnly: []string{l.cacheObjects},
		Env:      []string{"PATH=/usr/bin:/bin", "MULTICA_TASK_ID=t1"},
		Argv:     []string{"/bin/sh", "-c", script},
	}
}

func requireSandbox(t *testing.T, l *Launcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.SelfCheck(ctx); err != nil {
		t.Skipf("host cannot run the sandbox: %v", err)
	}
}

func runSandbox(t *testing.T, l *Launcher, spec Spec) (string, error) {
	t.Helper()
	run, err := l.Command(context.Background(), spec)
	if err != nil {
		return "", err
	}
	defer run.Close()
	var out bytes.Buffer
	run.Cmd.Stdout, run.Cmd.Stderr = &out, &out
	if err := run.Start(); err != nil {
		return "", err
	}
	waitErr := run.Cmd.Wait()
	return out.String(), waitErr
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeOff, "off": ModeOff, " Audit ": ModeAudit, "ON": ModeOn} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseMode("enforce"); err == nil {
		t.Error("ParseMode(enforce) accepted an unknown mode")
	}
}

func TestArgsMountsByConstruction(t *testing.T) {
	ly := newLayout(t)
	args, err := newLauncher().Args(ly.spec("true"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--unshare-user", "--unshare-pid", "--unshare-net", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup",
		"--die-with-parent", "--new-session",
		"--ro-bind /usr /usr",
		"--bind " + ly.work + " " + ly.work,
		"--bind " + ly.home + " " + ly.home,
		"--bind " + ly.tmp + " " + ly.tmp,
		"--ro-bind " + ly.cacheObjects + " " + ly.cacheObjects,
		"--chdir " + ly.work,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	for _, banned := range []string{"docker.sock", ".ssh", "github-apps", ".claude", ".codex", ly.otherWork, ly.cache + " "} {
		if strings.Contains(joined, banned) {
			t.Errorf("args mention %q:\n%s", banned, joined)
		}
	}
	if !strings.HasSuffix(joined, "-- /bin/sh -c true") {
		t.Errorf("command not last: %s", joined)
	}
}

func TestArgsKeepEnvValuesOutOfArgv(t *testing.T) {
	ly := newLayout(t)
	spec := ly.spec("true")
	spec.Env = append(spec.Env, "MULTICA_TOKEN=fake-run-token-value")
	args, err := newLauncher().Args(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "fake-run-token-value") {
		t.Error("env value leaked into the process command line")
	}
}

func TestArgsRejectUnsafeSpecs(t *testing.T) {
	ly := newLayout(t)
	fakeHome := t.TempDir()
	ssh := filepath.Join(fakeHome, ".ssh")
	if err := os.MkdirAll(ssh, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ly.root, "innocent")
	if err := os.Symlink(ssh, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Spec){
		"relative workdir":    func(s *Spec) { s.WorkDir = "work" },
		"unclean path":        func(s *Spec) { s.ReadOnly = []string{ly.cacheObjects + "/../objects"} },
		"root":                func(s *Spec) { s.ReadOnly = []string{"/"} },
		"ancestor of workdir": func(s *Spec) { s.ReadOnly = []string{ly.root} },
		"docker socket":       func(s *Spec) { s.ReadOnly = []string{"/var/run/docker.sock"} },
		"denied dir":          func(s *Spec) { s.ReadOnly = []string{ssh} },
		"inside denied dir":   func(s *Spec) { s.ReadOnly = []string{filepath.Join(ssh, "id")} },
		"symlink to denied":   func(s *Spec) { s.ReadOnly = []string{link} },
		"bad env name":        func(s *Spec) { s.Env = []string{"1BAD=x"} },
		"env without equals":  func(s *Spec) { s.Env = []string{"NOEQUALS"} },
		"no argv":             func(s *Spec) { s.Argv = nil },
		"no workdir":          func(s *Spec) { s.WorkDir = "" },
	}
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			l := newLauncher()
			l.Deny = append(DefaultDeny(), ssh)
			spec := ly.spec("true")
			cases[name](&spec)
			if args, err := l.Args(spec); err == nil {
				t.Errorf("accepted: %v", args)
			}
		})
	}
}

func TestOffModeRunsPlainCommand(t *testing.T) {
	ly := newLayout(t)
	l := newLauncher()
	l.Mode = ModeOff
	t.Setenv("MULTICA_PLAIN_PROBE", "inherited")
	out, err := runSandbox(t, l, ly.spec(`echo "$MULTICA_PLAIN_PROBE:$PWD"`))
	if err != nil {
		t.Fatal(err, out)
	}
	if want := "inherited:" + ly.work; strings.TrimSpace(out) != want {
		t.Errorf("off mode output %q, want %q", out, want)
	}
}

func TestAuditModeRunsPlainAndLogsWhatItWouldDo(t *testing.T) {
	ly := newLayout(t)
	var logs bytes.Buffer
	l := newLauncher()
	l.Mode = ModeAudit
	l.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	spec := ly.spec(`echo ran`)
	spec.ReadOnly = []string{"/var/run/docker.sock"}
	out, err := runSandbox(t, l, spec)
	if err != nil || strings.TrimSpace(out) != "ran" {
		t.Fatalf("audit must run the command unchanged: %q, %v", out, err)
	}
	if !strings.Contains(logs.String(), "would refuse") || !strings.Contains(logs.String(), "docker.sock") {
		t.Errorf("audit did not log the refusal:\n%s", logs.String())
	}
}

func TestOnModeRefusesWhenSelfCheckFails(t *testing.T) {
	ly := newLayout(t)
	l := newLauncher()
	l.BwrapPath = "/bin/false"
	marker := filepath.Join(ly.work, "ran")
	run, err := l.Command(context.Background(), ly.spec("touch "+marker))
	if !errors.Is(err, ErrSelfCheck) {
		t.Fatalf("err = %v, want ErrSelfCheck", err)
	}
	if run != nil {
		t.Error("got a Run despite failed self-check")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("command ran despite failed self-check")
	}
}

// T11 secret part: nothing outside the construction is reachable.
func TestSandboxHidesSecrets(t *testing.T) {
	l := newLauncher()
	requireSandbox(t, l)
	ly := newLayout(t)
	sentinel := filepath.Join(ly.root, "fake-home", ".config", "github-apps", "key.pem")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, sentinel, "sentinel")
	t.Setenv("GH_TOKEN", "fake-gh-token")
	t.Setenv("GITHUB_TOKEN", "fake-github-token")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/fake-agent.sock")

	script := `cat "` + sentinel + `" 2>&1; echo "gh=${GH_TOKEN-unset} github=${GITHUB_TOKEN-unset} ssh=${SSH_AUTH_SOCK-unset} home=$HOME tmp=$TMPDIR"; ls /home /var/run/docker.sock /run/docker.sock 2>&1`
	out, _ := runSandbox(t, l, ly.spec(script))
	if strings.Contains(out, "sentinel") && !strings.Contains(out, "No such file") {
		t.Errorf("sentinel readable:\n%s", out)
	}
	for _, want := range []string{"gh=unset", "github=unset", "ssh=unset", "home=" + ly.home, "tmp=" + ly.tmp} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "No such file") < 4 {
		t.Errorf("expected sentinel, /home and both docker.sock paths absent:\n%s", out)
	}
}

func TestSandboxIsolatesFilesystemAndNetwork(t *testing.T) {
	l := newLauncher()
	requireSandbox(t, l)
	ly := newLayout(t)
	script := strings.Join([]string{
		`cat ` + filepath.Join(ly.otherWork, "secret") + ` 2>&1`,
		`ls ` + ly.cache + ` 2>&1`,
		`cat ` + filepath.Join(ly.cacheObjects, "obj") + ` 2>&1`,
		`touch ` + filepath.Join(ly.cacheObjects, "new") + ` 2>&1`,
		`touch /usr/new 2>&1`,
		`echo ok > ` + filepath.Join(ly.work, "out") + ` && echo work-writable`,
		`echo ok > ` + filepath.Join(ly.home, "out") + ` && echo home-writable`,
		`echo ok > $TMPDIR/out && echo tmp-writable`,
		`awk 'NR>2 {sub(":","",$1); print "iface=" $1}' /proc/net/dev`,
	}, "\n")
	out, err := runSandbox(t, l, ly.spec(script))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "other-task") {
		t.Errorf("read another task workdir:\n%s", out)
	}
	if strings.Contains(out, "cache-meta") || strings.Contains(out, "worktrees") {
		t.Errorf("repo cache metadata visible:\n%s", out)
	}
	for _, want := range []string{"object", "work-writable", "home-writable", "tmp-writable", "iface=lo", "Read-only file system"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "iface=") != 1 {
		t.Errorf("network namespace has more than loopback:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ly.cacheObjects, "new")); err == nil {
		t.Error("sandbox wrote into the read-only cache objects dir")
	}
}

func TestRunScopeKillAndCleanup(t *testing.T) {
	l := newLauncher()
	requireSandbox(t, l)
	ly := newLayout(t)
	spec := ly.spec("sleep 30")
	run, err := l.Command(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	cg, err := os.ReadFile("/proc/" + strconv.Itoa(run.Cmd.Process.Pid) + "/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	dir := "/sys/fs/cgroup" + strings.TrimSpace(strings.TrimPrefix(string(cg), "0::"))
	if !strings.HasPrefix(filepath.Base(dir), "multica-run-") {
		t.Errorf("process is not in a run scope: %s", dir)
	}
	if err := run.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = run.Cmd.Wait()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("scope cgroup still present: %v", err)
	}
}

// M1: sandbox start p95 at most 100 ms over 50 starts.
func TestM1StartLatency(t *testing.T) {
	if raceEnabled {
		t.Skip("M1 is measured on a normal build; the race detector slows fork")
	}
	l := newLauncher()
	requireSandbox(t, l)
	ly := newLayout(t)
	var samples []time.Duration
	for i := 0; i < 50; i++ {
		begin := time.Now()
		if out, err := runSandbox(t, l, Spec{
			WorkDir: ly.work, TempDir: ly.tmp, Home: ly.home,
			Env: []string{"PATH=/usr/bin:/bin"}, Argv: []string{"/bin/true"},
		}); err != nil {
			t.Fatalf("start %d: %v\n%s", i, err, out)
		}
		samples = append(samples, time.Since(begin))
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })
	p95 := samples[len(samples)*95/100]
	t.Logf("M1 sandbox start+exit over 50 starts: min=%v p50=%v p95=%v max=%v", samples[0], samples[25], p95, samples[49])
	if p95 > 100*time.Millisecond {
		t.Errorf("M1 p95 %v exceeds 100ms", p95)
	}
}
