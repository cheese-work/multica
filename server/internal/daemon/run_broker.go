package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/multica-ai/multica/server/internal/daemon/broker"
	"github.com/multica-ai/multica/server/internal/daemon/repocache"
	"github.com/multica-ai/multica/server/internal/daemon/sandbox"
)

// BrokerConfig is the GitHub App the per-run push/read broker mints tokens
// with (CHE-2216 A4c). RepoIDs maps "owner/repo" (lower case) to the numeric
// repository ID: the claim carries none, and a token request must name one.
type BrokerConfig struct {
	AppID          string           `json:"app_id"`
	InstallationID int64            `json:"installation_id"`
	KeyFile        string           `json:"key_file"`
	APIBase        string           `json:"api_base"` // default https://api.github.com
	ReadMarginSec  int              `json:"read_margin_seconds"`
	RepoIDs        map[string]int64 `json:"repo_ids"`
}

// LoadBrokerConfig reads MULTICA_BROKER_CONFIG. An empty path means no broker.
func LoadBrokerConfig(path string) (*BrokerConfig, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c BrokerConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.AppID == "" || c.InstallationID <= 0 || c.KeyFile == "" || len(c.RepoIDs) == 0 {
		return nil, fmt.Errorf("%s: app_id, installation_id, key_file and repo_ids are required", path)
	}
	repos := make(map[string]int64, len(c.RepoIDs))
	for slug, id := range c.RepoIDs {
		repos[strings.ToLower(slug)] = id
	}
	c.RepoIDs = repos
	return &c, nil
}

func (c *BrokerConfig) tokens() (*broker.Tokens, error) {
	raw, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read App key: %w", err)
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(raw) // PKCS#1 or PKCS#8
	if err != nil {
		return nil, errors.New("parse App key failed") // never echo key material
	}
	api := c.APIBase
	if api == "" {
		api = "https://api.github.com"
	}
	return &broker.Tokens{APIBase: api, AppID: c.AppID, InstallationID: c.InstallationID, Key: key,
		ReadMargin: time.Duration(c.ReadMarginSec) * time.Second}, nil
}

// multicaExe is the binary the shims call back into. A test replaces it.
var multicaExe = os.Executable

// runBroker is one run's broker. The shims and the socket path are fixed when
// the run launches; the listener starts at the first checkout, when the task
// branch, its base commit and the cache objects exist (the pin cannot be built
// earlier). Close revokes every token the run still holds.
type runBroker struct {
	ShimDir, Socket string // host paths: bind ShimDir read-only and pass Socket as sandbox.Spec.BrokerSocket

	cfg    *BrokerConfig
	tokens *broker.Tokens
	log    *slog.Logger
	dir    string

	mu     sync.Mutex
	b      *broker.Broker
	closed bool
}

// applyRunBroker is the per-run hook next to applyRestrictedHome. Only
// enforcement on with a broker config changes anything: git and gh wrappers
// go first on the run's PATH and MULTICA_BROKER_SOCKET names the socket. Off,
// audit and an unconfigured daemon keep today's behaviour; the run then has no
// GitHub credential at all (the env allowlist drops them).
//
// isolated says the run sits behind enforced filesystem isolation that exposes
// only its own broker socket. Without it every run shares the daemon UID, so
// any run could connect to another run's socket: refuse to start a broker.
func applyRunBroker(mode sandbox.Mode, isolated bool, cfg *BrokerConfig, agentEnv map[string]string, log *slog.Logger) (*runBroker, error) {
	if mode != sandbox.ModeOn {
		return nil, nil
	}
	if !isolated {
		log.Warn("sandbox: runs are not filesystem-isolated; broker not started, run has no GitHub push or read path")
		return nil, nil
	}
	if cfg == nil {
		log.Warn("sandbox: no MULTICA_BROKER_CONFIG; run has no GitHub push or read path")
		return nil, nil
	}
	tokens, err := cfg.tokens()
	if err != nil {
		return nil, err
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	realGH, err := exec.LookPath("gh")
	if err != nil {
		return nil, err
	}
	mc, err := multicaExe()
	if err != nil {
		return nil, err
	}
	// Short path: a Unix socket path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "mbr-")
	if err != nil {
		return nil, err
	}
	rb := &runBroker{cfg: cfg, tokens: tokens, log: log, dir: dir,
		ShimDir: filepath.Join(dir, "bin"), Socket: filepath.Join(dir, "s", "b.sock")}
	for _, d := range []string{rb.ShimDir, filepath.Dir(rb.Socket)} {
		if err = os.Mkdir(d, 0o700); err != nil {
			break
		}
	}
	if err == nil {
		err = broker.InstallShims(rb.ShimDir, realGit, realGH, mc)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	path := agentEnv["PATH"]
	if path == "" {
		path = os.Getenv("PATH")
	}
	agentEnv["PATH"] = rb.ShimDir + string(os.PathListSeparator) + path
	agentEnv[broker.SocketEnv] = rb.Socket
	return rb, nil
}

// brokerCheckout is what the first repo checkout of a run tells the broker.
type brokerCheckout struct {
	TaskID, RepoURL, Branch, Base, Objects string
	Bases                                  []string
	Policy                                 func(context.Context) ([]string, error)
}

// start pins the broker to the run's first checkout; later calls change nothing.
func (rb *runBroker) start(c brokerCheckout) error {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if rb.closed || rb.b != nil {
		return nil
	}
	slug, ok := strings.CutPrefix(repocache.RepoIdentity(c.RepoURL), "github.com/")
	id, known := rb.cfg.RepoIDs[slug]
	if !ok || !known {
		return fmt.Errorf("broker: no GitHub repository ID configured for %s", c.RepoURL)
	}
	b, err := broker.New(broker.Pin{
		Repo: strconv.FormatInt(id, 10), Ref: "refs/heads/" + c.Branch, Base: c.Base,
		RemoteURL: c.RepoURL, Objects: c.Objects, Tokens: rb.tokens, Slug: slug, Bases: c.Bases,
		Task: c.TaskID, Policy: c.Policy,
	}, rb.Socket, rb.log)
	if err != nil {
		return err
	}
	rb.b = b
	return nil
}

// Close ends the run's broker, which revokes every token it minted, and
// removes the shims and socket. Safe on nil and on repeat.
func (rb *runBroker) Close() {
	if rb == nil {
		return
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if rb.closed {
		return
	}
	rb.closed = true
	if rb.b != nil {
		if err := rb.b.Close(); err != nil {
			rb.log.Warn("broker close", "error", err)
		}
	}
	_ = os.RemoveAll(rb.dir)
}

// startRunBroker pins the run's broker to a finished checkout. A failure is
// logged, not returned: the checkout itself worked, and a run without a broker
// has no GitHub push or read path, which is the safe state.
func (d *Daemon) startRunBroker(t activeRepoCheckoutTask, req repoCheckoutRequest, res *repocache.WorktreeResult) {
	if t.Broker == nil || res == nil || res.BranchName == "" {
		return
	}
	// A kept checkout with local work may sit on a branch the agent picked, not
	// this task's own (or the server-pinned) branch. Keep the work, grant nothing.
	if res.Kept == repocache.KeptLocalWork {
		d.logger.Warn("run broker not started", "task_id", t.TaskID, "reason", "checkout kept on a branch not owned by this task")
		return
	}
	bare := d.repoCache.BarePath(req.WorkspaceID, req.URL)
	def := gitOut(bare, "symbolic-ref", "--short", "HEAD")
	head := gitOut(res.Path, "rev-parse", "HEAD")
	base := head
	if res.Kept != "" && def != "" {
		if mb := gitOut(bare, "merge-base", head, "refs/remotes/origin/"+def); mb != "" {
			base = mb
		}
	}
	bases := []string{def}
	if issueBase := d.taskRepoDefaultRef(req.WorkspaceID, req.TaskID, req.URL); issueBase != "" && issueBase != def {
		bases = append(bases, issueBase)
	}
	if def == "" || head == "" {
		d.logger.Warn("run broker not started", "task_id", t.TaskID, "reason", "default branch or HEAD unresolved")
		return
	}
	if err := t.Broker.start(brokerCheckout{
		TaskID: t.TaskID, RepoURL: req.URL, Branch: res.BranchName, Base: base,
		Objects: repocache.ObjectsDir(bare), Bases: bases, Policy: d.client.BrokerPolicy(req.WorkspaceID),
	}); err != nil {
		d.logger.Warn("run broker not started", "task_id", t.TaskID, "error", err)
	}
}

func gitOut(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runIsolated is false until agent runs start through sandbox.Launcher with
// Spec.BrokerSocket (no production path does today). Flip it with that wiring.
const runIsolated = false
