package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/selfexec"
)

// Fork update source (CHE-1012). On Linux and macOS the daemon and
// `multica update` take their binary from the daemon artifact that
// cheese-work/multica's ci.yml uploads on a push to main, never from upstream
// release binaries. An artifact is a candidate only when it belongs to the
// newest CI run that completed successfully for a push to main in this exact
// repository, and its digest (read from the authenticated API, not from the
// zip) matches the bytes downloaded.

// ForkRepo is the only repository an update may come from.
const ForkRepo = "cheese-work/multica"

const (
	forkWorkflowFile  = "ci.yml"
	forkWorkflowPath  = ".github/workflows/" + forkWorkflowFile
	forkBranch        = "main"
	forkBinaryName    = "multica"
	forkMaxBytes      = 256 << 20
	forkAPITimeout    = 15 * time.Second
	forkProbeTimeout  = 10 * time.Second
	forkTokenEnv      = "MULTICA_UPDATE_GITHUB_TOKEN"
	forkArtifactStem  = "multica-daemon-"
	forkMacOSPausedBy = "macOS daemon CI is paused (no free macOS runners), so no macOS artifact is published"
)

var (
	// ErrForkUpToDate: the running commit is the newest eligible main build.
	ErrForkUpToDate = errors.New("already on the latest successful main build")
	// ErrForkNotNewer: the newest eligible build is behind or diverged from the running commit.
	ErrForkNotNewer = errors.New("newest successful main build is not ahead of the running commit")
	// ErrForkNoArtifact: no usable artifact for this platform. The installed daemon is kept.
	ErrForkNoArtifact = errors.New("no eligible fork artifact")
)

// ClientCommit is the build commit stamped into this binary (set from main).
var ClientCommit = "unknown"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ValidCommit reports whether c can anchor an ordering check against the fork.
func ValidCommit(c string) bool { return commitPattern.MatchString(c) }

// ForkUpdateSupported reports whether goos takes updates from the fork source.
// Windows keeps its existing behaviour (out of scope for CHE-1012).
func ForkUpdateSupported(goos string) bool { return goos == "linux" || goos == "darwin" }

// ForkArtifactName is the CI artifact name for a platform.
func ForkArtifactName(goos, goarch string) string {
	return forkArtifactStem + goos + "-" + goarch
}

// ForkUpdate is a fully qualified update candidate.
type ForkUpdate struct {
	RunID      int64
	RunURL     string
	HeadSHA    string
	ArtifactID int64
	Name       string
	Digest     string // lowercase hex sha256 of the artifact zip
}

func (u *ForkUpdate) String() string {
	return fmt.Sprintf("%s (commit %s, CI run %d, artifact %d, sha256 %s)", u.Name, u.HeadSHA, u.RunID, u.ArtifactID, u.Digest)
}

type forkRepoRef struct {
	FullName string `json:"full_name"`
}

type forkRun struct {
	ID             int64       `json:"id"`
	HTMLURL        string      `json:"html_url"`
	Path           string      `json:"path"`
	Status         string      `json:"status"`
	Conclusion     string      `json:"conclusion"`
	Event          string      `json:"event"`
	HeadBranch     string      `json:"head_branch"`
	HeadSHA        string      `json:"head_sha"`
	Repository     forkRepoRef `json:"repository"`
	HeadRepository forkRepoRef `json:"head_repository"`
}

type forkArtifact struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Expired     bool   `json:"expired"`
	Digest      string `json:"digest"`
	WorkflowRun struct {
		ID               int64  `json:"id"`
		HeadBranch       string `json:"head_branch"`
		HeadSHA          string `json:"head_sha"`
		RepositoryID     int64  `json:"repository_id"`
		HeadRepositoryID int64  `json:"head_repository_id"`
	} `json:"workflow_run"`
}

// ineligible reports why r may not be an update source, or "" when it may.
func (r *forkRun) ineligible() string {
	switch {
	case r.Status != "completed" || r.Conclusion != "success":
		return fmt.Sprintf("status %q conclusion %q", r.Status, r.Conclusion)
	case r.Event != "push" || r.HeadBranch != forkBranch:
		return fmt.Sprintf("event %q branch %q", r.Event, r.HeadBranch)
	case r.Path != forkWorkflowPath:
		return fmt.Sprintf("workflow path %q", r.Path)
	case !strings.EqualFold(r.Repository.FullName, ForkRepo) || !strings.EqualFold(r.HeadRepository.FullName, ForkRepo):
		return fmt.Sprintf("repository %q head repository %q", r.Repository.FullName, r.HeadRepository.FullName)
	case len(r.HeadSHA) != 40 || !ValidCommit(r.HeadSHA):
		return fmt.Sprintf("head sha %q", r.HeadSHA)
	}
	return ""
}

// forkGet performs an authenticated GET against the API. The token goes only
// to the API host: Go drops Authorization on the cross-host redirect to the
// artifact blob store, and CheckRedirect refuses plaintext hops outright.
func forkGet(ctx context.Context, rawURL string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := forkToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if next.URL.Host != via[0].URL.Host {
				next.Header.Del("Authorization")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		// url.Error carries only the URL; the token is a header and never appears here.
		return nil, err
	}
	return resp, nil
}

func forkToken() string {
	for _, k := range []string{forkTokenEnv, "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func forkGetJSON(ctx context.Context, path string, out any) (int, error) {
	resp, err := forkGet(ctx, releaseAPIBaseURL()+"/repos/"+ForkRepo+path, forkAPITimeout)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			hint = fmt.Sprintf(" (private repository? set %s to a token with actions:read)", forkTokenEnv)
		}
		return resp.StatusCode, fmt.Errorf("GitHub API %s returned %d%s", path, resp.StatusCode, hint)
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// ResolveForkUpdate finds the update candidate for goos/goarch and checks it is
// ahead of currentCommit. It never falls back to any other source.
func ResolveForkUpdate(ctx context.Context, goos, goarch, currentCommit string) (*ForkUpdate, error) {
	if !ValidCommit(currentCommit) {
		return nil, fmt.Errorf("running binary has no usable build commit (%q); reinstall it with scripts/install-cli-from-ref.sh once", currentCommit)
	}

	var runs struct {
		Runs []forkRun `json:"workflow_runs"`
	}
	q := url.Values{"branch": {forkBranch}, "event": {"push"}, "status": {"success"}, "per_page": {"1"}}
	if _, err := forkGetJSON(ctx, "/actions/workflows/"+forkWorkflowFile+"/runs?"+q.Encode(), &runs); err != nil {
		return nil, fmt.Errorf("list CI runs: %w", err)
	}
	// Only the newest successful run is considered: a newer main commit without
	// an artifact means "unavailable", not "install something older".
	if len(runs.Runs) == 0 {
		return nil, fmt.Errorf("%w: no successful CI run on %s", ErrForkNoArtifact, forkBranch)
	}
	run := runs.Runs[0]
	if why := run.ineligible(); why != "" {
		return nil, fmt.Errorf("%w: newest CI run %d is not eligible (%s)", ErrForkNoArtifact, run.ID, why)
	}

	name := ForkArtifactName(goos, goarch)
	var arts struct {
		Artifacts []forkArtifact `json:"artifacts"`
	}
	if _, err := forkGetJSON(ctx, fmt.Sprintf("/actions/runs/%d/artifacts?name=%s", run.ID, url.QueryEscape(name)), &arts); err != nil {
		return nil, fmt.Errorf("list artifacts for CI run %d: %w", run.ID, err)
	}
	var art *forkArtifact
	for i := range arts.Artifacts {
		a := &arts.Artifacts[i]
		w := a.WorkflowRun
		if a.Name == name && !a.Expired && w.ID == run.ID && w.HeadSHA == run.HeadSHA && w.HeadBranch == forkBranch &&
			w.RepositoryID != 0 && w.RepositoryID == w.HeadRepositoryID {
			art = a
			break
		}
	}
	if art == nil {
		reason := fmt.Sprintf("CI run %d (commit %s) has no unexpired %s artifact", run.ID, run.HeadSHA, name)
		if goos == "darwin" {
			reason += "; " + forkMacOSPausedBy
		}
		return nil, fmt.Errorf("%w: %s", ErrForkNoArtifact, reason)
	}
	hexSum, ok := strings.CutPrefix(art.Digest, "sha256:")
	if !ok || len(hexSum) != 64 {
		return nil, fmt.Errorf("%w: artifact %d has no sha256 digest in the API (%q)", ErrForkNoArtifact, art.ID, art.Digest)
	}
	if _, err := hex.DecodeString(hexSum); err != nil {
		return nil, fmt.Errorf("%w: artifact %d digest is not hex", ErrForkNoArtifact, art.ID)
	}
	u := &ForkUpdate{RunID: run.ID, RunURL: run.HTMLURL, HeadSHA: run.HeadSHA, ArtifactID: art.ID, Name: name, Digest: strings.ToLower(hexSum)}

	if strings.HasPrefix(run.HeadSHA, currentCommit) {
		return nil, ErrForkUpToDate
	}
	var cmp struct {
		Status string `json:"status"`
	}
	code, err := forkGetJSON(ctx, "/compare/"+currentCommit+"..."+run.HeadSHA, &cmp)
	if err != nil {
		if code == http.StatusNotFound {
			return nil, fmt.Errorf("running commit %s is not in %s history; reinstall once with scripts/install-cli-from-ref.sh: %w", currentCommit, ForkRepo, err)
		}
		return nil, fmt.Errorf("compare commits: %w", err)
	}
	switch cmp.Status {
	case "ahead":
		return u, nil
	case "identical":
		return nil, ErrForkUpToDate
	default:
		return nil, fmt.Errorf("%w (compare status %q)", ErrForkNotNewer, cmp.Status)
	}
}

// ApplyForkUpdate downloads u, verifies it, and atomically replaces the running
// executable. Any failure before the final rename leaves the installed binary intact.
func ApplyForkUpdate(ctx context.Context, u *ForkUpdate, timeout time.Duration) (string, error) {
	exePath, err := selfexec.Resolve()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	if exePath, err = filepath.EvalSymlinks(exePath); err != nil {
		return "", fmt.Errorf("resolve symlink: %w", err)
	}
	return applyForkUpdateTo(ctx, u, timeout, exePath)
}

func applyForkUpdateTo(ctx context.Context, u *ForkUpdate, timeout time.Duration, exePath string) (string, error) {
	resp, err := forkGet(ctx, fmt.Sprintf("%s/repos/%s/actions/artifacts/%d/zip", releaseAPIBaseURL(), ForkRepo, u.ArtifactID), updateDownloadTimeoutOrDefault(timeout))
	if err != nil {
		return "", fmt.Errorf("download artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download artifact: HTTP %d (artifact expired or token lacks actions:read)", resp.StatusCode)
	}
	zipData, err := io.ReadAll(io.LimitReader(resp.Body, forkMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("download artifact: %w", err)
	}
	if len(zipData) > forkMaxBytes {
		return "", fmt.Errorf("artifact exceeds %d bytes", forkMaxBytes)
	}
	if err := verifyAssetSHA256(zipData, u.Digest, u.Name); err != nil {
		return "", fmt.Errorf("verify download: %w", err)
	}

	bin, err := extractForkBinary(zipData)
	if err != nil {
		return "", fmt.Errorf("extract binary: %w", err)
	}
	if err := probeForkBinary(ctx, bin, u.HeadSHA); err != nil {
		return "", fmt.Errorf("probe binary: %w", err)
	}
	if err := installBinary(exePath, bin); err != nil {
		return "", err
	}
	return fmt.Sprintf("Installed %s over %s", u, exePath), nil
}

// extractForkBinary returns the single regular file named "multica" from zipData,
// rejecting any other entry shape (traversal, symlinks, extras, oversize).
func extractForkBinary(zipData []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("zip reader: %w", err)
	}
	if len(zr.File) != 1 {
		return nil, fmt.Errorf("expected exactly one entry, got %d", len(zr.File))
	}
	f := zr.File[0]
	if f.Name != forkBinaryName || !f.Mode().IsRegular() {
		return nil, fmt.Errorf("unexpected entry %q (mode %v)", f.Name, f.Mode())
	}
	if f.UncompressedSize64 == 0 || f.UncompressedSize64 > forkMaxBytes {
		return nil, fmt.Errorf("entry size %d out of range", f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, forkMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) != f.UncompressedSize64 {
		return nil, errors.New("entry size mismatch")
	}
	return data, nil
}

var probeCommitPattern = regexp.MustCompile(`\(commit: ([0-9a-f]+),`)

// probeForkBinary runs the candidate's --version and requires it to execute on
// this platform and report the commit the artifact was attributed to. Catches a
// wrong-architecture or wrong-commit binary before it replaces the daemon.
func probeForkBinary(ctx context.Context, bin []byte, headSHA string) error {
	dir, err := os.MkdirTemp("", "multica-probe-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, forkBinaryName)
	if err := os.WriteFile(path, bin, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, forkProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("candidate does not run on %s/%s: %w", runtime.GOOS, runtime.GOARCH, err)
	}
	var got string
	if m := probeCommitPattern.FindSubmatch(out); m != nil {
		got = string(m[1])
	}
	if len(got) < 7 || !strings.HasPrefix(headSHA, got) {
		return fmt.Errorf("candidate does not report commit %s: %q", headSHA, strings.TrimSpace(string(out)))
	}
	return nil
}
