package agent

import (
	"os/exec"
	"strings"
	"testing"
)

// secretNames are daemon-held credentials a restricted run must never inherit.
var secretNames = []string{
	"GH_TOKEN", "GITHUB_TOKEN", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_MESSAGING_TOKEN",
	"PRIVATE_KEY_PATH", "SSH_AUTH_SOCK", "MULTICA_TOKEN", "AWS_SECRET_ACCESS_KEY",
}

func setDaemonEnv(t *testing.T) {
	t.Helper()
	for _, name := range secretNames {
		t.Setenv(name, "sentinel-"+name)
	}
	t.Setenv("HOME", "/daemon/home")
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("LC_ALL", "C.UTF-8")
}

func TestRestrictedEnvDropsDaemonSecrets(t *testing.T) {
	setDaemonEnv(t)
	SetRestrictedEnv(true)
	t.Cleanup(func() { SetRestrictedEnv(false) })

	env := strings.Join(buildEnv(map[string]string{"HOME": "/run/home", "MULTICA_TASK_ID": "t1"}), "\n")
	for _, name := range secretNames {
		if strings.Contains(env, name+"=") {
			t.Errorf("restricted env leaks %s", name)
		}
	}
	for _, want := range []string{"HOME=/run/home", "MULTICA_TASK_ID=t1", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"} {
		if !strings.Contains(env, want) {
			t.Errorf("restricted env missing %q", want)
		}
	}
	if strings.Contains(env, "/daemon/home") {
		t.Error("restricted env inherited the daemon HOME")
	}
}

func TestEnvStaysAmbientWhenOff(t *testing.T) {
	setDaemonEnv(t)
	env := strings.Join(buildEnv(nil), "\n")
	if !strings.Contains(env, "GH_TOKEN=sentinel-GH_TOKEN") {
		t.Error("mode off must keep the ambient environment")
	}
}

// A fake agent binary (env) run with every builder's base: no secret reaches it.
func TestRestrictedEnvReachesChildProcess(t *testing.T) {
	setDaemonEnv(t)
	SetRestrictedEnv(true)
	t.Cleanup(func() { SetRestrictedEnv(false) })

	cmd := exec.Command("env")
	cmd.Env = buildEnv(map[string]string{"HOME": "/run/home"})
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range secretNames {
		if strings.Contains(string(out), name+"=") {
			t.Errorf("child process sees %s", name)
		}
	}
	if !strings.Contains(string(out), "HOME=/run/home") {
		t.Error("child lost the per-run HOME")
	}
}

func TestSplitEnvReportsNamesOnly(t *testing.T) {
	kept, dropped := SplitEnv([]string{"PATH=/bin", "GH_TOKEN=abc", "HOME=/h"})
	if len(kept) != 1 || kept[0] != "PATH=/bin" {
		t.Errorf("kept = %v", kept)
	}
	if strings.Join(dropped, ",") != "GH_TOKEN,HOME" {
		t.Errorf("dropped = %v", dropped)
	}
}
