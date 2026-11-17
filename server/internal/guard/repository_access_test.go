package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRepositoryAccessBeforeToolExecution(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		args       []string
		deny       bool
	}{
		{"denied comment", "gh", []string{"pr", "comment", "7", "-R", "tevo-studio/sa041-downloader-5", "--body", "x"}, true},
		{"denied create", "gh", []string{"pr", "create", "-R", "tevo-studio/sa041-downloader-5", "--title", "fix: x"}, true},
		{"denied push URL", "git", []string{"push", "https://github.com/tevo-studio/sa041-downloader-5.git", "HEAD"}, true},
		{"denied push SSH", "git", []string{"push", "git@github.com:tevo-studio/sa041-downloader-5.git", "HEAD"}, true},
		{"allowed comment", "gh", []string{"pr", "comment", "7", "-R", "TEVO-STUDIO/SA039-ASTROLOGY", "--body", "x"}, false},
		{"other organization", "gh", []string{"pr", "comment", "7", "-R", "cheese-work/multica", "--body", "x"}, false},
		{"read allowed", "gh", []string{"pr", "view", "7", "-R", "tevo-studio/sa041-downloader-5"}, false},
		{"attached repo", "gh", []string{"pr", "comment", "7", "-Rtevo-studio/sa041-downloader-5", "--body", "x"}, true},
		{"URL overrides repo", "gh", []string{"pr", "comment", "https://github.com/tevo-studio/sa041-downloader-5/pull/7", "-R", "tevo-studio/sa039-astrology", "--body", "x"}, true},
		{"global repo flag", "gh", []string{"-Rtevo-studio/sa041-downloader-5", "pr", "comment", "7", "--body", "x"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPolicy()
			// Decode through the existing public policy shape so the initial red
			// measures an actual unchecked write, rather than a missing symbol.
			if err := json.Unmarshal([]byte(`{"repository_allowlist":{"tevo-studio":["sa039-astrology"]}}`), p); err != nil {
				t.Fatal(err)
			}
			f := newFake()
			var out, errs, log bytes.Buffer
			e := Engine{Policy: p, Runner: repositoryRunner{f}, Out: &out, Err: &errs, Log: &log}
			code, err := e.Exec(context.Background(), tc.tool, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if tc.deny {
				if code != 2 || len(f.streamed) != 0 || len(f.prs) != 0 || !strings.Contains(errs.String(), "allowlist") {
					t.Fatalf("denied repo executed: code=%d streamed=%v PRs=%d stderr=%q", code, f.streamed, len(f.prs), errs.String())
				}
				if !strings.Contains(log.String(), `"action":"refused"`) {
					t.Fatalf("missing refusal evidence: %s", &log)
				}
			} else if code != 0 || len(f.streamed) != 1 {
				t.Fatalf("allowed call refused: code=%d stderr=%q", code, errs.String())
			}
		})
	}
}

func TestCanonicalRepositoryPolicy(t *testing.T) {
	raw, err := os.ReadFile("../../../config/tevo-prod1-guard.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Cheese's latest attached list supersedes the initial team-only roster.
	for repo, allowed := range map[string]bool{"tevo-studio/sa039-astrology": true, "tevo-studio/code-android-template": true, "tevo-studio/mMonetize": true, "tevo-studio/sa041-downloader-5": false, "tevo-studio/st008-palios": true, "tevo-studio/tt025-ai-home-design": true, "tevo-studio/sa079-reelora": true, "tevo-studio/tt016-vpn2": false, "tevo-studio/sa061-flash-light-2": false} {
		if got := p.Resolve(Target{Repo: repo}).CheckRepository(repo) == nil; got != allowed {
			t.Errorf("%s allowed=%v", repo, got)
		}
	}
}

// Use real, local Git config resolution; Stream still records instead of pushing.
type repositoryRunner struct{ *fakeEnv }

func (r repositoryRunner) Run(ctx context.Context, tool string, args ...string) (Result, error) {
	if tool == "git" {
		return (ExecRunner{Real: func(string) string { return os.Getenv("MULTICA_GUARD_REAL_GIT") }}).Run(ctx, tool, args...)
	}
	return r.fakeEnv.Run(ctx, tool, args...)
}

func TestPushUsesEffectiveDestination(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	for _, tc := range []struct {
		name   string
		config [][2]string
		args   []string
		deny   bool
	}{
		{"allowed origin", nil, []string{"push", "origin", "HEAD"}, false},
		{"pushurl", [][2]string{{"remote.origin.pushurl", "https://github.com/tevo-studio/sa041-downloader-5.git"}}, []string{"push", "origin"}, true},
		{"multiple pushurls", [][2]string{{"remote.origin.pushurl", "https://github.com/tevo-studio/sa039-astrology.git"}, {"remote.origin.pushurl", "https://github.com/tevo-studio/sa041-downloader-5.git"}}, []string{"push"}, true},
		{"pushremote", [][2]string{{"remote.other.url", "git@github.com:tevo-studio/sa041-downloader-5.git"}, {"remote.pushDefault", "other"}}, []string{"push"}, true},
		{"rewrite", [][2]string{{"url.https://github.com/tevo-studio/sa041-downloader-5.git.pushInsteadOf", "https://github.com/tevo-studio/sa039-astrology.git"}}, []string{"push", "origin"}, true},
		{"explicit rewrite", [][2]string{{"url.https://github.com/tevo-studio/sa041-downloader-5.git.pushInsteadOf", "approved:"}}, []string{"push", "approved:"}, true},
		{"command config", nil, []string{"-c", "remote.origin.pushurl=https://github.com/tevo-studio/sa041-downloader-5.git", "push", "origin"}, true},
		{"repo option plus positional", nil, []string{"push", "--repo=origin", "https://github.com/tevo-studio/sa041-downloader-5.git"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			git := func(args ...string) {
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				if b, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git config: %s: %v", b, err)
				}
			}
			git("init", "-q")
			git("remote", "add", "origin", "https://github.com/tevo-studio/sa039-astrology.git")
			for _, c := range tc.config {
				git("config", "--add", c[0], c[1])
			}
			f := newFake()
			p := DefaultPolicy()
			p.RepositoryAllowlist = map[string][]string{"tevo-studio": {"sa039-astrology"}}
			var out, errs bytes.Buffer
			e := Engine{Policy: p, Runner: repositoryRunner{f}, Out: &out, Err: &errs}
			code, err := e.Exec(context.Background(), "git", append([]string{"-C", dir}, tc.args...))
			if err != nil || (code == 2) != tc.deny || (len(f.streamed) == 0) != tc.deny {
				t.Fatalf("code=%d err=%v streamed=%v stderr=%s", code, err, f.streamed, &errs)
			}
		})
	}
}
