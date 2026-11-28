package broker

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The credential journal (plan CHE-10291, G1) records every token the broker
// hands to a run, sealed on disk, so a process that dies before it revokes
// the token can have it revoked later. The token is its own bearer for
// DELETE /installation/token, so recovery needs no App key.
//
// Layout of one root (one per daemon profile):
//
//	journal.key        32 random bytes, 0600, made once with O_EXCL; never
//	                   derived from an App key, never replaced while entries exist
//	<sha256(token)[:16]>  one 0600 file per token: a clear header plus the
//	                   token sealed with AES-256-GCM, the header being the
//	                   additional data
//
// Each entry has an owner instance (one per daemon process). Inside the owning
// process the journal tracks a state per entry and guards it with one mutex:
//
//	held -> retiring -> released -> sweeping -> deleted
//	sweeping -> released (a failed revoke); any state -> deleted (a confirmed revoke)
//
// A sweep revokes only entries of a prior instance and `released` entries. It
// never touches `held`, `retiring` or `sweeping` ones.

const (
	journalKeyFile    = "journal.key"
	journalFormat     = 1
	journalNameLen    = 16 // hex characters of sha256(token) in an entry name
	journalTempMark   = ".tmp-"
	journalGarbageAge = 2 * time.Hour
	// journalMaxLife bounds the expiry Put accepts. A GitHub installation token
	// lives an hour, so a file older than journalGarbageAge holds a dead token.
	journalMaxLife       = time.Hour + 30*time.Minute
	journalRevokeTimeout = 15 * time.Second
)

var errJournalKeyLost = errors.New("journal: journal.key is missing while entries exist; refusing to create a new key")

type jstate int

const (
	stHeld jstate = iota + 1
	stRetiring
	stReleased
	stSweeping
)

func (s jstate) String() string {
	switch s {
	case stHeld:
		return "held"
	case stRetiring:
		return "retiring"
	case stReleased:
		return "released"
	case stSweeping:
		return "sweeping"
	}
	return "unknown"
}

// JournalOptions configures OpenJournal.
type JournalOptions struct {
	// Instance is this process's random 128-bit ID (NewInstanceID). Empty
	// means a fresh one.
	Instance string
	Logger   *slog.Logger
	// HTTP is the client for sweep revokes; nil means a client that does not
	// follow redirects.
	HTTP *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	insecureHTTP bool // tests only: allow an http:// API base
}

// JournalEntry is what the broker records for one token.
type JournalEntry struct {
	Token   string
	Expires time.Time
	APIBase string // https URL the token is revoked against
	TaskID  string
}

// JournalReceipt names one outcome by case and by sha256(token)[:8], never by
// token. Case is one of swept, expired_unrevoked, revoke_failed, unseal_failed.
type JournalReceipt struct {
	Case  string
	Token string
}

// SweepResult counts what one Sweep did.
type SweepResult struct {
	Revoked          int // revoked and deleted
	ExpiredUnrevoked int // deleted at expiry with no request sent
	UnsealFailed     int // could not be authenticated; no request sent, kept until expiry
	Failed           int // a revoke attempt failed; kept
	Orphan           int // same-instance entry with no state; not swept
	Skipped          int // held, retiring or sweeping; not eligible
	Receipts         []JournalReceipt
}

// JournalStatus is the daemon status view: nothing here is a secret.
type JournalStatus struct {
	Pending        int
	EarliestExpiry time.Time
	UnsealFailed   int
	Orphan         int
}

type jrec struct {
	state   jstate
	expires time.Time
}

// Journal is the store. It is safe for concurrent use.
type Journal struct {
	root     string
	instance string
	log      *slog.Logger
	httpc    *http.Client
	now      func() time.Time
	insecure bool
	writeKey func(*os.File, []byte) error // tests only: replaces writeKeyFile

	mu      sync.Mutex
	key     []byte
	keyID   string
	own     map[string]*jrec // entries put by this instance, by file name
	claimed map[string]bool  // prior-instance entries a sweep has claimed
	logged  map[string]bool  // names already logged as unauthenticatable
}

type jheader struct {
	V     int    `json:"v"`
	KeyID string `json:"kid"`
	Owner string `json:"owner"`
	Exp   int64  `json:"exp"`
	Base  string `json:"base"`
	Task  string `json:"task"`
}

type jfile struct {
	H jheader `json:"h"`
	N []byte  `json:"n"`
	C []byte  `json:"c"`
}

// NewInstanceID returns a random 128-bit instance ID in hex.
func NewInstanceID() string {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		panic("journal: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// OpenJournal opens the journal at root. It never creates the root or the key:
// a root that does not exist means there is nothing to recover. Put creates
// both, at the first hand-out.
func OpenJournal(root string, o JournalOptions) (*Journal, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("journal: root %q is not absolute and clean", root)
	}
	j := &Journal{
		root: root, instance: o.Instance, log: o.Logger, httpc: o.HTTP, now: o.Now, insecure: o.insecureHTTP,
		own: map[string]*jrec{}, claimed: map[string]bool{}, logged: map[string]bool{},
	}
	if j.instance == "" {
		j.instance = NewInstanceID()
	}
	if j.log == nil {
		j.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if j.now == nil {
		j.now = time.Now
	}
	if j.httpc == nil {
		j.httpc = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.loadKeyLocked(); err != nil {
		return nil, err
	}
	return j, nil
}

// Instance returns this journal's owner ID.
func (j *Journal) Instance() string { return j.instance }

func entryName(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:journalNameLen]
}

func isEntryName(n string) bool {
	if len(n) != journalNameLen {
		return false
	}
	_, err := hex.DecodeString(n)
	return err == nil
}

func (j *Journal) path(name string) string { return filepath.Join(j.root, name) }

// loadKeyLocked reads journal.key if it exists. A missing key leaves j.key nil.
func (j *Journal) loadKeyLocked() error {
	b, err := os.ReadFile(j.path(journalKeyFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("journal: read key: %w", err)
	}
	if len(b) != 32 {
		j.log.Error("journal: journal.key has the wrong size; entries cannot be unsealed", "bytes", len(b))
		return nil
	}
	sum := sha256.Sum256(b)
	j.key, j.keyID = b, hex.EncodeToString(sum[:])[:8]
	return nil
}

// loadKeyBestEffortLocked loads the key for a read-only caller. A read error
// leaves the key nil, so entries fail authentication and are kept.
func (j *Journal) loadKeyBestEffortLocked() {
	if err := j.loadKeyLocked(); err != nil {
		j.log.Error("journal: cannot read journal.key", "error", err)
	}
}

func (j *Journal) hasEntries() bool {
	des, err := os.ReadDir(j.root)
	if err != nil {
		return false
	}
	for _, de := range des {
		if isEntryName(de.Name()) {
			return true
		}
	}
	return false
}

// createKeyLocked makes journal.key, once, with O_EXCL. It refuses when entries
// exist: those are sealed under a key that is gone, and a new one would only
// hide that. It also fails when a key file exists, so it never replaces one.
func (j *Journal) createKeyLocked() error {
	if j.hasEntries() {
		return errJournalKeyLost
	}
	if err := os.MkdirAll(j.root, 0o700); err != nil {
		return fmt.Errorf("journal: create root: %w", err)
	}
	if err := os.Chmod(j.root, 0o700); err != nil {
		return fmt.Errorf("journal: chmod root: %w", err)
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return fmt.Errorf("journal: random key: %w", err)
	}
	f, err := os.OpenFile(j.path(journalKeyFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("journal: create key: %w", err)
	}
	write := j.writeKey
	if write == nil {
		write = writeKeyFile
	}
	err = write(f, key)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(j.path(journalKeyFile))
		return fmt.Errorf("journal: write key: %w", err)
	}
	if err := syncDir(j.root); err != nil {
		return err
	}
	sum := sha256.Sum256(key)
	j.key, j.keyID = key, hex.EncodeToString(sum[:])[:8]
	return nil
}

// writeKeyFile makes the key file 0600, writes all of key and syncs it.
func writeKeyFile(f *os.File, key []byte) error {
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(key); err != nil {
		return err
	}
	return f.Sync()
}

func (j *Journal) createKey() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.createKeyLocked()
}

func (j *Journal) ensureKeyLocked() error {
	if j.key != nil {
		return nil
	}
	if err := j.loadKeyLocked(); err != nil {
		return err
	}
	if j.key != nil {
		return nil
	}
	return j.createKeyLocked()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("journal: open dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("journal: sync dir: %w", err)
	}
	return nil
}

func (j *Journal) checkBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("journal: API base is not a plain URL")
	}
	if u.Scheme != "https" && !(j.insecure && u.Scheme == "http") {
		return errors.New("journal: API base must be https")
	}
	return nil
}

func aadOf(h jheader) []byte {
	b, _ := json.Marshal(h)
	return b
}

func newGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// Put seals the entry, writes it (fsynced, 0600) and registers it as held, all
// before it returns, so the broker can hand the token out afterwards. A failure
// means the token must not be handed out.
func (j *Journal) Put(e JournalEntry) error {
	if e.Token == "" || e.Expires.IsZero() {
		return errors.New("journal: entry needs a token and an expiry")
	}
	if err := j.checkBase(e.APIBase); err != nil {
		return err
	}
	if e.Expires.After(j.now().Add(journalMaxLife)) {
		return errors.New("journal: entry expiry is further out than a token can live")
	}
	name := entryName(e.Token)

	j.mu.Lock()
	if err := j.ensureKeyLocked(); err != nil {
		j.mu.Unlock()
		return err
	}
	if j.own[name] != nil {
		j.mu.Unlock()
		return errors.New("journal: token already journaled")
	}
	key, keyID := j.key, j.keyID
	j.mu.Unlock()

	h := jheader{V: journalFormat, KeyID: keyID, Owner: j.instance, Exp: e.Expires.Unix(), Base: e.APIBase, Task: e.TaskID}
	gcm, err := newGCM(key)
	if err != nil {
		return fmt.Errorf("journal: cipher: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("journal: random nonce: %w", err)
	}
	data, err := json.Marshal(jfile{H: h, N: nonce, C: gcm.Seal(nil, nonce, []byte(e.Token), aadOf(h))})
	if err != nil {
		return err
	}

	tmp := j.path(journalTempMark + name + "-" + NewInstanceID()[:8])
	if err := writeSynced(tmp, data); err != nil {
		os.Remove(tmp)
		return err
	}
	j.mu.Lock()
	if j.own[name] != nil {
		j.mu.Unlock()
		os.Remove(tmp)
		return errors.New("journal: token already journaled")
	}
	if err := os.Rename(tmp, j.path(name)); err != nil {
		j.mu.Unlock()
		os.Remove(tmp)
		return fmt.Errorf("journal: place entry: %w", err)
	}
	j.own[name] = &jrec{state: stHeld, expires: time.Unix(h.Exp, 0)}
	j.mu.Unlock()
	return syncDir(j.root)
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("journal: create entry: %w", err)
	}
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("journal: write entry: %w", err)
	}
	return nil
}

func (j *Journal) move(token string, from, to jstate) error {
	name := entryName(token)
	j.mu.Lock()
	defer j.mu.Unlock()
	r := j.own[name]
	if r == nil {
		return fmt.Errorf("journal: no entry %s of this instance", name[:8])
	}
	if r.state != from {
		return fmt.Errorf("journal: entry %s is %s, not %s", name[:8], r.state, from)
	}
	r.state = to
	return nil
}

// Retire moves a held entry to retiring: the broker has taken the token out of
// use and its own revoke or retry is running. A sweep leaves it alone.
func (j *Journal) Retire(token string) error { return j.move(token, stHeld, stRetiring) }

// Release gives a retiring entry up to the sweeper: the broker's retries ended
// and the token may still be live.
func (j *Journal) Release(token string) error { return j.move(token, stRetiring, stReleased) }

// Delete removes the entry after a confirmed revoke, in any state. Deleting an
// entry that is already gone is not an error.
func (j *Journal) Delete(token string) error {
	name := entryName(token)
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.own, name)
	delete(j.claimed, name)
	if err := os.Remove(j.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("journal: delete entry: %w", err)
	}
	return nil
}

func parseEntry(raw []byte) (jfile, error) {
	var f jfile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return jfile{}, errors.New("malformed entry")
	}
	if f.H.V != journalFormat {
		return jfile{}, errors.New("unknown entry format")
	}
	return f, nil
}

// unseal authenticates the whole header and returns the token.
func unseal(key []byte, keyID string, f jfile) (string, error) {
	if key == nil {
		return "", errors.New("no journal key")
	}
	if f.H.KeyID != keyID {
		return "", errors.New("entry sealed under another key")
	}
	gcm, err := newGCM(key)
	if err != nil || len(f.N) != gcm.NonceSize() {
		return "", errors.New("bad nonce")
	}
	tok, err := gcm.Open(nil, f.N, f.C, aadOf(f.H))
	if err != nil {
		return "", errors.New("authentication failed")
	}
	return string(tok), nil
}

func (j *Journal) currentKey() ([]byte, string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.key == nil {
		j.loadKeyBestEffortLocked()
	}
	return j.key, j.keyID
}

func (j *Journal) receipt(res *SweepResult, c, name string) {
	res.Receipts = append(res.Receipts, JournalReceipt{Case: c, Token: name[:8]})
	j.log.Info("journal receipt", "case", c, "token", name[:8])
}

// unsealFailed counts the entry, logs one error per entry for the life of the
// journal, and keeps the entry until its expiry.
func (j *Journal) unsealFailed(res *SweepResult, name, why string) {
	res.UnsealFailed++
	j.mu.Lock()
	first := !j.logged[name]
	j.logged[name] = true
	j.mu.Unlock()
	if first {
		j.log.Error("journal entry cannot be authenticated; kept until expiry, no request sent", "token", name[:8], "reason", why)
	}
}

func (j *Journal) unclaim(name string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if r := j.own[name]; r != nil && r.state == stSweeping {
		r.state = stReleased
	}
	delete(j.claimed, name)
}

// Sweep revokes every eligible entry: entries of a prior instance and released
// entries of this one. Held, retiring and sweeping entries get no request.
func (j *Journal) Sweep(ctx context.Context) SweepResult {
	var res SweepResult
	des, err := os.ReadDir(j.root)
	if err != nil {
		return res
	}
	for _, de := range des {
		if ctx.Err() != nil {
			break
		}
		name := de.Name()
		if strings.HasPrefix(name, journalTempMark) {
			j.dropStaleTemp(name)
			continue
		}
		if isEntryName(name) && !de.IsDir() {
			j.sweepOne(ctx, name, &res)
		}
	}
	return res
}

func (j *Journal) dropStaleTemp(name string) {
	if info, err := os.Stat(j.path(name)); err == nil && j.now().Sub(info.ModTime()) > journalGarbageAge {
		os.Remove(j.path(name))
	}
}

func (j *Journal) sweepOne(ctx context.Context, name string, res *SweepResult) {
	now := j.now()
	j.mu.Lock()
	var raw []byte
	switch rec := j.own[name]; {
	case rec != nil:
		if !now.Before(rec.expires) {
			os.Remove(j.path(name))
			delete(j.own, name)
			j.mu.Unlock()
			res.ExpiredUnrevoked++
			j.receipt(res, "expired_unrevoked", name)
			return
		}
		if rec.state != stReleased {
			j.mu.Unlock()
			res.Skipped++
			return
		}
		rec.state = stSweeping
	case j.claimed[name]:
		j.mu.Unlock()
		res.Skipped++
		return
	default:
		b, err := os.ReadFile(j.path(name))
		if err != nil {
			j.mu.Unlock()
			return // deleted since the listing
		}
		// Authenticate before any decision: expiry and owner are header
		// fields, and an altered header must not delete or hide an entry.
		f, perr := parseEntry(b)
		if perr == nil {
			if j.key == nil {
				j.loadKeyBestEffortLocked()
			}
			_, perr = unseal(j.key, j.keyID, f)
		}
		if perr != nil {
			// No header value can be trusted, so age comes from the file: a
			// Put refuses an expiry beyond journalMaxLife, far less than journalGarbageAge.
			drop := false
			if info, serr := os.Stat(j.path(name)); serr == nil && now.Sub(info.ModTime()) > journalGarbageAge {
				os.Remove(j.path(name))
				drop = true
			}
			j.mu.Unlock()
			if drop {
				res.ExpiredUnrevoked++
				j.receipt(res, "expired_unrevoked", name)
			} else {
				j.unsealFailed(res, name, perr.Error())
			}
			return
		}
		if !now.Before(time.Unix(f.H.Exp, 0)) {
			os.Remove(j.path(name))
			j.mu.Unlock()
			res.ExpiredUnrevoked++
			j.receipt(res, "expired_unrevoked", name)
			return
		}
		if f.H.Owner == j.instance {
			// Put registers and writes in one critical section, so this cannot
			// happen through the API. A run might still hold it: do not sweep.
			j.mu.Unlock()
			res.Orphan++
			j.log.Error("journal entry of this instance has no state; not swept", "token", name[:8])
			return
		}
		j.claimed[name] = true
		raw = b
	}
	j.mu.Unlock()
	j.revokeClaimed(ctx, name, raw, res)
}

func (j *Journal) revokeClaimed(ctx context.Context, name string, raw []byte, res *SweepResult) {
	if raw == nil {
		b, err := os.ReadFile(j.path(name))
		if err != nil {
			j.unclaim(name)
			return // deleted by a confirmed revoke while we waited
		}
		raw = b
	}
	f, err := parseEntry(raw)
	if err != nil {
		j.unclaim(name)
		j.unsealFailed(res, name, err.Error())
		return
	}
	key, keyID := j.currentKey()
	tok, err := unseal(key, keyID, f)
	if err == nil {
		err = j.checkBase(f.H.Base)
	}
	if err != nil {
		j.unclaim(name)
		j.unsealFailed(res, name, err.Error())
		return
	}
	if rerr := j.revoke(ctx, f.H.Base, tok); rerr != nil {
		j.unclaim(name)
		res.Failed++
		j.receipt(res, "revoke_failed", name)
		j.log.Warn("journal revoke failed; entry kept", "token", name[:8], "error", rerr)
		return
	}
	j.mu.Lock()
	os.Remove(j.path(name))
	delete(j.own, name)
	delete(j.claimed, name)
	j.mu.Unlock()
	res.Revoked++
	j.receipt(res, "swept", name)
}

// revoke is DELETE {base}/installation/token with the token as its own bearer.
// 204 and 401 both mean the token is dead.
func (j *Journal) revoke(ctx context.Context, base, token string) error {
	ctx, cancel := context.WithTimeout(ctx, journalRevokeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(base, "/")+"/installation/token", nil)
	if err != nil {
		return errors.New("bad revoke request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := j.httpc.Do(req)
	if err != nil {
		return errors.New("revoke request failed")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("revoke token: status %d", resp.StatusCode)
	}
	return nil
}

// Status counts the entries on disk for the daemon status line.
func (j *Journal) Status() JournalStatus {
	var st JournalStatus
	des, err := os.ReadDir(j.root)
	if err != nil {
		return st
	}
	key, keyID := j.currentKey()
	for _, de := range des {
		name := de.Name()
		if !isEntryName(name) || de.IsDir() {
			continue
		}
		raw, err := os.ReadFile(j.path(name))
		if err != nil {
			continue
		}
		st.Pending++
		f, err := parseEntry(raw)
		if err != nil {
			st.UnsealFailed++
			continue
		}
		if exp := time.Unix(f.H.Exp, 0); st.EarliestExpiry.IsZero() || exp.Before(st.EarliestExpiry) {
			st.EarliestExpiry = exp
		}
		if _, err := unseal(key, keyID, f); err != nil {
			st.UnsealFailed++
			continue
		}
		j.mu.Lock()
		known := j.own[name] != nil || j.claimed[name]
		j.mu.Unlock()
		if f.H.Owner == j.instance && !known {
			st.Orphan++
		}
	}
	return st
}
