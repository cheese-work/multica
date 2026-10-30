package cli

import (
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

// Fork update source (CHE-1012, CHE-1980). On Linux and macOS the daemon and
// `multica update` take their binary from the Multica server they already talk
// to, never from GitHub or upstream release binaries. The server image carries
// daemon builds made from the server's own commit; the daemon follows the
// deployed server. The sha256 in the metadata is verified against the bytes
// before install and a probe checks the candidate runs and reports the
// commit. The digest detects corruption in transit, not a compromised server.

const (
	forkBinaryName   = "multica"
	forkMaxBytes     = 256 << 20
	forkAPITimeout   = 15 * time.Second
	forkProbeTimeout = 10 * time.Second
	forkMetadataPath = "/api/daemon/update"
	forkBinaryPath   = "/api/daemon/update/binary"
)

var (
	// ErrForkUpToDate: the running commit is the server's build commit.
	ErrForkUpToDate = errors.New("already on the server's build")
	// ErrForkNoArtifact: the server has no usable build for this platform. The installed daemon is kept.
	ErrForkNoArtifact = errors.New("no daemon build available from the server")
)

// ClientCommit is the build commit stamped into this binary (set from main).
var ClientCommit = "unknown"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ValidCommit reports whether c can anchor a comparison with the server's build commit.
func ValidCommit(c string) bool { return commitPattern.MatchString(c) }

// ForkUpdateSupported reports whether goos takes updates from the server.
// Windows keeps its existing behaviour (out of scope for CHE-1012).
func ForkUpdateSupported(goos string) bool { return goos == "linux" || goos == "darwin" }

// ForkUpdate is a fully qualified update candidate on one server.
type ForkUpdate struct {
	ServerURL string // HTTP(S) base URL of the server that offered it
	Token     string // credential for ServerURL; never printed
	HeadSHA   string // the server's build commit
	Version   string
	Name      string // platform key, e.g. linux-amd64
	Digest    string // lowercase hex sha256 of the binary
	Size      int64
}

func (u *ForkUpdate) String() string {
	return fmt.Sprintf("%s %s (commit %s, sha256 %s)", u.Name, u.Version, u.HeadSHA, u.Digest)
}

type forkMetadata struct {
	Commit   string `json:"commit"`
	Version  string `json:"version"`
	Binaries map[string]struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"binaries"`
}

// forkGet performs an authenticated GET against the server. The default client
// drops the Authorization header on a redirect to another host.
func forkGet(ctx context.Context, serverURL, token, path string, timeout time.Duration) (*http.Response, error) {
	base, err := url.Parse(strings.TrimRight(serverURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("server URL %q is not an http(s) URL", serverURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+path, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return (&http.Client{Timeout: timeout}).Do(req)
}

// ResolveForkUpdate asks the server which build to run and checks it differs
// from currentCommit. There is no ordering check: a server rolled back to an
// older commit takes its daemons back with it.
func ResolveForkUpdate(ctx context.Context, serverURL, token, goos, goarch, currentCommit string) (*ForkUpdate, error) {
	if !ValidCommit(currentCommit) {
		return nil, fmt.Errorf("running binary has no usable build commit (%q); reinstall it with scripts/install-cli-from-ref.sh once", currentCommit)
	}
	resp, err := forkGet(ctx, serverURL, token, forkMetadataPath, forkAPITimeout)
	if err != nil {
		return nil, fmt.Errorf("fetch update metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: server answered HTTP %d for %s", ErrForkNoArtifact, resp.StatusCode, forkMetadataPath)
	}
	var meta forkMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode update metadata: %w", err)
	}
	if len(meta.Commit) != 40 || !ValidCommit(meta.Commit) {
		return nil, fmt.Errorf("%w: server build commit %q", ErrForkNoArtifact, meta.Commit)
	}
	name := goos + "-" + goarch
	bin, ok := meta.Binaries[name]
	if !ok {
		return nil, fmt.Errorf("%w: server has no %s build", ErrForkNoArtifact, name)
	}
	if _, err := hex.DecodeString(bin.SHA256); err != nil || len(bin.SHA256) != 64 {
		return nil, fmt.Errorf("%w: %s build has no valid sha256", ErrForkNoArtifact, name)
	}
	if bin.Size <= 0 || bin.Size > forkMaxBytes {
		return nil, fmt.Errorf("%w: %s build size %d out of range", ErrForkNoArtifact, name, bin.Size)
	}
	if strings.HasPrefix(meta.Commit, currentCommit) {
		return nil, ErrForkUpToDate
	}
	return &ForkUpdate{ServerURL: serverURL, Token: token, HeadSHA: meta.Commit, Version: meta.Version,
		Name: name, Digest: strings.ToLower(bin.SHA256), Size: bin.Size}, nil
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
	return ApplyForkUpdateTo(ctx, u, timeout, exePath)
}

// ApplyForkUpdateTo is ApplyForkUpdate for an explicit executable path.
func ApplyForkUpdateTo(ctx context.Context, u *ForkUpdate, timeout time.Duration, exePath string) (string, error) {
	goos, goarch, _ := strings.Cut(u.Name, "-")
	q := url.Values{"os": {goos}, "arch": {goarch}}
	resp, err := forkGet(ctx, u.ServerURL, u.Token, forkBinaryPath+"?"+q.Encode(), updateDownloadTimeoutOrDefault(timeout))
	if err != nil {
		return "", fmt.Errorf("download binary: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download binary: HTTP %d from the server", resp.StatusCode)
	}
	bin, err := io.ReadAll(io.LimitReader(resp.Body, forkMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("download binary: %w", err)
	}
	if len(bin) > forkMaxBytes {
		return "", fmt.Errorf("binary exceeds %d bytes", forkMaxBytes)
	}
	if err := verifyAssetSHA256(bin, u.Digest, u.Name); err != nil {
		return "", fmt.Errorf("verify download: %w", err)
	}
	if err := probeForkBinary(ctx, bin, u.HeadSHA); err != nil {
		return "", fmt.Errorf("probe binary: %w", err)
	}
	if err := installBinary(exePath, bin); err != nil {
		return "", err
	}
	return fmt.Sprintf("Installed %s over %s", u, exePath), nil
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
