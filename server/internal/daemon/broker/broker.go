// Package broker is the host side of the sandbox push path (CHE-2216 slice
// A3a). The run holds no GitHub credential. It hands a git bundle of its task
// branch to a per-run Unix socket; the broker checks the ref and repository
// against the pin it was built with, then pushes from the host.
package broker

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxBundle caps one request body. Bundles stream to disk, never into memory.
const maxBundle = 1 << 30

// Pin is everything a run may push to. It comes from the daemon, never from
// the run.
type Pin struct {
	Repo      string `json:"repo"` // numeric repository ID from the claim
	Ref       string `json:"ref"`  // the only ref that may be pushed, refs/heads/...
	Base      string `json:"base"` // commit the task branch started from (bundle prerequisite)
	RemoteURL string `json:"-"`    // push target; the run never supplies a URL
	Objects   string `json:"-"`    // cache object directory, borrowed so bundle prerequisites resolve
	// Tokens, Slug and Bases switch on GitHub access: a token per push and the
	// run's own-PR operations. Slug is owner/repo; Bases are the branches a PR
	// may target (the default branch, or a stack base named on the issue).
	Tokens *Tokens  `json:"-"`
	Slug   string   `json:"-"`
	Bases  []string `json:"-"`
	// Deadline bounds one operation; zero means 10 minutes.
	Deadline time.Duration `json:"-"`
	Task     string        `json:"-"` // task ID, written into every receipt
	// Policy returns the workspace protected-path patterns, read from the server
	// on every push so an admin edit applies at once. An error or an empty list
	// refuses the push: no policy is never "no protected paths".
	Policy func(context.Context) ([]string, error) `json:"-"`
	// ReceiptLog, when set, is a JSON-lines file each receipt is appended to.
	ReceiptLog string `json:"-"`
	// MaxCommits caps the commits one push may add; zero means the default.
	MaxCommits int `json:"-"`
	// MaxPaths caps the paths one push may change, and so the work the
	// protected-path check does; zero means the default.
	MaxPaths int `json:"-"`
	// Elevation, when set, makes every push and PR operation an elevated one.
	Elevation *Elevation `json:"-"`
	// Synthetic, when set, marks an elevated run whose checkout is a synthetic
	// base (plan 3.5): Base is then Synthetic.Root, and a push is translated
	// onto the real history before any check or write.
	Synthetic *Synthetic `json:"-"`
}

// Synthetic ties a synthetic root commit to the real commit whose tree it holds.
type Synthetic struct {
	Root   string // the synthetic root, SyntheticBase.Root
	Parent string // the real commit that replaces it as a parent: SyntheticBase.Source
}

// gitEnv keeps host git config, hooks and prompts out of broker git runs.
var gitEnv = []string{
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "LC_ALL=C",
}

// Broker serves one run's pin on one Unix socket.
type Broker struct {
	pin    Pin
	log    *slog.Logger
	ln     net.Listener
	srv    *http.Server
	socket string
	mu     sync.Mutex // one operation at a time per run
	bg     context.Context
	stop   context.CancelFunc // ends background token-revoke retries
	wg     sync.WaitGroup     // those retries
	opsMu  sync.Mutex         // guards closed and ops.Add against Close
	closed bool
	ops    sync.WaitGroup // requests in flight, including their token revoke

	rmu      sync.Mutex
	receipts []Receipt

	pinMu    sync.RWMutex // guards Base, Synthetic and baseRepo against a base refresh
	baseRepo string       // bare repo holding refreshed default-branch tips; "" until the first refresh

	fmu     sync.Mutex // guards fenced, open, changed and the elevOp fields
	fenced  bool
	open    map[*elevOp]struct{} // authorized, not yet acknowledged by the server
	changed chan struct{}        // closed and replaced when open or its state changes

	refmu     sync.Mutex            // one read-token refresh at a time
	rdmu      sync.Mutex            // guards reads
	reads     []grant               // read tokens handed to the run, revoked by Close
	pending   int                   // mints in flight that hold a slot under maxReadTokens
	unrevoked map[string]*staleRead // replaced read tokens whose revoke has not succeeded yet, by token id

	beforeWrite, afterWrite func() // test hooks around the remote write
}

// New starts a broker listening on socket (mode 0600).
func New(p Pin, socket string, log *slog.Logger) (*Broker, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("broker: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("broker: %w", err)
	}
	b := &Broker{pin: p, log: log, ln: ln, socket: socket, open: map[*elevOp]struct{}{}, changed: make(chan struct{})}
	b.bg, b.stop = context.WithCancel(context.Background())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pin", b.handlePin)
	mux.HandleFunc("POST /push", b.track(b.handlePush))
	mux.HandleFunc("POST /pr", b.track(b.handlePR))
	mux.HandleFunc("POST /readtoken", b.track(b.handleReadToken))
	mux.HandleFunc("POST /readtoken/refresh", b.track(b.handleReadRefresh))
	mux.HandleFunc("POST /readtoken/current", b.track(b.handleReadCurrent))
	mux.HandleFunc("POST /base-refresh", b.track(b.handleRefresh))
	b.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := b.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Error("broker serve", "error", err)
		}
	}()
	return b, nil
}

// track counts a request so Close can wait for it and its token revoke.
func (b *Broker) track(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.opsMu.Lock()
		if b.closed {
			b.opsMu.Unlock()
			http.Error(w, "broker is closing", http.StatusServiceUnavailable)
			return
		}
		b.ops.Add(1)
		b.opsMu.Unlock()
		defer b.ops.Done()
		h(w, r)
	}
}

// Close stops the broker and removes the socket. It returns after every
// request in flight has revoked its token. A token still waiting to be
// revoked gets one last try, then an alert.
func (b *Broker) Close() error {
	b.opsMu.Lock()
	b.closed = true
	b.opsMu.Unlock()
	err := b.srv.Close()
	_ = os.Remove(b.socket)
	b.ops.Wait()
	b.revokeReads()
	b.stop()
	b.wg.Wait()
	b.revokeLeftovers()
	if b.baseRepo != "" {
		_ = os.RemoveAll(b.baseRepo)
	}
	return err
}

func (p Pin) validate() error {
	switch {
	case p.Repo == "":
		return errors.New("broker: pin has no repository")
	case p.RemoteURL == "":
		return errors.New("broker: pin has no remote")
	case !strings.HasPrefix(p.Ref, "refs/heads/") || !validRef(p.Ref):
		return fmt.Errorf("broker: pin ref %q is not a branch", p.Ref)
	case !filepath.IsAbs(p.Objects):
		return errors.New("broker: pin objects directory is not absolute")
	case p.Tokens != nil:
		if _, err := p.repoID(); err != nil {
			return err
		}
		if !slugRE.MatchString(p.Slug) || len(p.Bases) == 0 {
			return errors.New("broker: pin needs an owner/repo slug and at least one base branch for GitHub access")
		}
	case p.Policy == nil:
		return errors.New("broker: pin has no protected-path policy source")
	}
	return nil
}

var slugRE = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

func (p Pin) repoID() (int64, error) {
	id, err := strconv.ParseInt(p.Repo, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("broker: pin repository %q is not a numeric ID", p.Repo)
	}
	return id, nil
}

func (p Pin) deadline() time.Duration { return or(p.Deadline, operationDeadline) }

func validRef(ref string) bool {
	return exec.Command("git", "check-ref-format", ref).Run() == nil
}

// handlePin tells the in-sandbox client what to bundle.
func (b *Broker) handlePin(w http.ResponseWriter, _ *http.Request) {
	b.pinMu.RLock()
	defer b.pinMu.RUnlock()
	_ = json.NewEncoder(w).Encode(b.pin)
}

func (b *Broker) handlePush(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	sha, status, err := b.push(r)
	b.log.Info("broker push", "task", b.pin.Task, "repo", b.pin.Repo, "ref", b.pin.Ref, "sha", sha, "status", status, "elapsed", time.Since(began), "error", err)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"ref": b.pin.Ref, "sha": sha})
}

// push runs the pin checks, unpacks the bundle in a scratch repository and
// pushes the one pinned ref. It forces only with a lease: the "lease" query
// value is the remote tip the caller expects to replace. It returns an HTTP
// status for a refusal or failure.
func (b *Broker) push(r *http.Request) (sha string, status int, err error) {
	q := r.URL.Query()
	if q.Get("ref") != b.pin.Ref {
		return "", http.StatusForbidden, errors.New("ref is not this run's task branch")
	}
	if repo := q.Get("repo"); repo != "" && repo != b.pin.Repo {
		return "", http.StatusForbidden, errors.New("repository is not this run's repository")
	}
	lease := q.Get("lease")
	if lease != "" && !validOID(lease) {
		return "", http.StatusBadRequest, errors.New("lease must be a full commit ID")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx := r.Context()
	patterns, err := b.pin.Policy(ctx)
	if err != nil || len(patterns) == 0 {
		b.log.Error("broker protected-path policy unavailable", "task", b.pin.Task, "error", err)
		return "", http.StatusServiceUnavailable, errors.New("protected-path policy unavailable; push refused")
	}
	// The token covers every network step: the default-branch lookup, the
	// protected-path fetch and the push. The deadline starts once it is live.
	var auth []string
	if b.pin.Tokens != nil {
		g, status, err := b.mint(ctx, false)
		if err != nil {
			return "", status, err
		}
		defer b.revoke(g)
		auth = basicAuthEnv(g.token)
	}
	ctx, cancel := context.WithTimeout(ctx, b.pin.deadline())
	defer cancel()
	expired := func(sha string) (string, int, error) {
		return sha, http.StatusGatewayTimeout, fmt.Errorf("push: operation deadline of %v passed", b.pin.deadline())
	}
	defRef, err := b.defaultBranch(ctx, auth)
	if ctx.Err() == context.DeadlineExceeded {
		return expired("")
	}
	if err != nil {
		return "", http.StatusServiceUnavailable, err
	}
	if defRef == b.pin.Ref {
		return "", http.StatusForbidden, errors.New("the task branch is the repository default branch")
	}
	scratch, err := os.MkdirTemp("", "multica-broker-")
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	defer os.RemoveAll(scratch)

	bundle := filepath.Join(scratch, "in.bundle")
	if err := saveBody(bundle, http.MaxBytesReader(nil, r.Body, maxBundle)); err != nil {
		return "", http.StatusBadRequest, err
	}
	work := filepath.Join(scratch, "work.git")
	if out, err := b.git(ctx, "", nil, "init", "--bare", "--template=", work); err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("init: %s", out)
	}
	if err := os.MkdirAll(filepath.Join(work, "objects", "info"), 0o755); err != nil {
		return "", http.StatusInternalServerError, err
	}
	if err := os.WriteFile(filepath.Join(work, "objects", "info", "alternates"), b.alternates(), 0o644); err != nil {
		return "", http.StatusInternalServerError, err
	}
	heads, err := bundleHeads(bundle)
	if err != nil {
		return "", http.StatusBadRequest, err
	}
	if len(heads) != 1 || heads[0].ref != b.pin.Ref {
		return "", http.StatusBadRequest, errors.New("bundle must hold exactly the task branch")
	}
	sha = heads[0].sha
	if b.pin.Synthetic != nil {
		if status, err := b.seedRoot(ctx, work); err != nil {
			return "", status, err
		}
	}
	if out, err := b.git(ctx, work, nil, "-c", "transfer.fsckObjects=true", "bundle", "unbundle", bundle); err != nil {
		return "", http.StatusBadRequest, fmt.Errorf("unbundle: %s", b.scrub(firstLine(out)))
	}
	var runSHA map[string]string
	if b.pin.Synthetic != nil {
		if sha, runSHA, status, err = b.translate(ctx, work, sha, filepath.Join(scratch, "msg")); err != nil {
			return "", status, err
		}
	}
	if status, err := b.checkProtected(ctx, work, defRef, sha, patterns, auth); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return expired(sha)
		}
		return sha, status, err
	}
	commits, err := b.commitReceipts(ctx, work, sha, runSHA)
	if errors.Is(err, errTooManyCommits) {
		return sha, http.StatusRequestEntityTooLarge, err
	}
	if err != nil {
		return sha, http.StatusInternalServerError, err
	}
	bundleSum, err := fileSHA256(bundle)
	if err != nil {
		return sha, http.StatusInternalServerError, err
	}
	op, status, err := b.authorize(ctx, inputHash("push", b.pin.Repo, b.pin.Ref, sha, lease, bundleSum))
	if err != nil {
		return sha, status, err
	}
	defer func() { b.finish(op, err) }()
	if err := b.write(op); err != nil {
		return sha, http.StatusForbidden, err
	}
	args := []string{"push", b.pin.RemoteURL, sha + ":" + b.pin.Ref}
	if lease != "" {
		args = []string{"push", "--force-with-lease=" + b.pin.Ref + ":" + lease, b.pin.RemoteURL, sha + ":" + b.pin.Ref}
	}
	if out, err := b.git(ctx, work, auth, args...); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return expired(sha)
		}
		err = fmt.Errorf("push: %s", b.scrub(pushReason(out)))
		if strings.Contains(out, "rejected]") { // git names a ref the remote refused
			err = notApplied{err}
		}
		return sha, http.StatusConflict, err
	}
	b.record(Receipt{Task: b.pin.Task, Repo: b.pin.Repo, Ref: b.pin.Ref, Time: time.Now().UTC(), Forced: lease != "", Commits: commits})
	return sha, http.StatusOK, nil
}

// basicAuthEnv hands a token to git as an HTTP header through the environment,
// so it never appears in a command line or in .git/config.
func basicAuthEnv(token string) []string {
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))}
}

// mint returns a one-repository token for the pinned repository and the HTTP
// status to answer with if that fails. The operation deadline starts once the
// token exists, because that is when the credential is live.
func (b *Broker) mint(ctx context.Context, pr bool) (grant, int, error) {
	perms := map[string]string{"contents": "write"}
	if pr {
		perms["pull_requests"] = "write"
	}
	return b.mintWith(ctx, perms)
}

func (b *Broker) mintWith(ctx context.Context, perms map[string]string) (grant, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, _ := b.pin.repoID() // checked in New
	g, err := b.pin.Tokens.mint(ctx, id, perms)
	if err != nil {
		return grant{}, http.StatusBadGateway, err
	}
	b.log.Info("broker token minted", "repo", b.pin.Repo, "token", g.id(), "expires_at", g.expires, "perms", perms)
	return g, 0, nil
}

// git runs git with the hardened environment plus env; dir may be empty.
func (b *Broker) git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"--git-dir", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(append(os.Environ(), gitEnv...), env...)
	cmd.WaitDelay = time.Second // a hook that outlives the killed git must not hold the deadline open
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

type head struct{ sha, ref string }

// bundleHeads reads the ref list from a bundle's text header, so no git
// process is needed to learn what the bundle claims to hold.
func bundleHeads(path string) ([]head, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	if !sc.Scan() || (sc.Text() != "# v2 git bundle" && sc.Text() != "# v3 git bundle") {
		return nil, errors.New("not a git bundle")
	}
	var heads []head
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			return heads, nil
		case line[0] == '-' || line[0] == '@': // prerequisite or capability
		default:
			sha, ref, ok := strings.Cut(line, " ")
			if !ok {
				return nil, errors.New("malformed bundle header")
			}
			heads = append(heads, head{sha, ref})
		}
	}
	return nil, errors.New("malformed bundle header")
}

func saveBody(path string, body io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// pushReason picks the line of git push output that says why it failed.
func pushReason(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "!") || strings.HasPrefix(l, "error:") || strings.HasPrefix(l, "fatal:") {
			return l
		}
	}
	return firstLine(out)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
