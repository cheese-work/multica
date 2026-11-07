// Package readproxy is the host side of GitHub reads for an elevated run
// (CHE-2216 slice B2a, plan 3.4). The run's `gh` shim reaches it only
// through a Unix socket. The proxy holds the read token, fetches one object
// with a single GraphQL request, hashes the exact body bytes, and returns
// them only when the server admits that revision; otherwise it returns a
// stub (ID, permalink, SHA-256). Any error returns no body bytes.
package readproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// SocketEnv names the variable that holds the proxy socket path in the run.
const SocketEnv = "MULTICA_READPROXY_SOCK"

const (
	maxResponse    = 4 << 20
	defaultLookup  = 5 * time.Second
	defaultGraphQL = "https://api.github.com/graphql"
)

// Comments, reviews and review comments have an admission path (CHE-2214);
// PR title and body and CI logs do not (external.go).
var kinds = map[string]bool{"IssueComment": true, "PullRequestReview": true, "PullRequestReviewComment": true}

var objectID = regexp.MustCompile(`^[A-Za-z0-9_=-]{1,128}$`)

const query = `query($id: ID!) { node(id: $id) {
  __typename
  ... on IssueComment { id url body lastEditedAt editor { ... on User { databaseId } } }
  ... on PullRequestReview { id url body state lastEditedAt editor { ... on User { databaseId } } }
  ... on PullRequestReviewComment { id url body lastEditedAt editor { ... on User { databaseId } } }
} }`

// Check is what the proxy asks the server about one object it read. The
// server applies the 5.2 rule to the editor fields.
type Check struct {
	IssueID, TaskID  string
	Kind, ObjectID   string
	State            string // reviews only
	BodySHA256       string
	LastEditedAt     string // as GitHub returned it; empty when never edited
	EditorPresent    bool
	EditorDatabaseID *int64 // nil for no editor, or an editor that is not a user
}

// Admitter answers whether the server admits this exact object revision
// (an admitted intake revision, or a scoped snapshot approval).
type Admitter interface {
	Admitted(ctx context.Context, c Check) (bool, error)
}

// OwnMessager is the broker's receipt check (broker.Broker implements it): it
// reports whether msg, the current bytes of a commit message, hash to the
// receipt of a commit the broker pushed for this run under sha.
type OwnMessager interface {
	OwnMessage(sha string, msg []byte) bool
}

// Result is one read's answer: full text, or a stub with no body.
type Result struct {
	Full   bool   `json:"full"`
	Kind   string `json:"kind,omitempty"`
	ID     string `json:"id"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Body   string `json:"body,omitempty"`
}

// Proxy serves reads for one elevated run. Its socket is private to that run,
// so the socket is the run's identity.
type Proxy struct {
	IssueID, TaskID string
	GraphQLURL      string // default https://api.github.com/graphql
	Token           func(context.Context) (string, error)
	Admit           Admitter
	Approve         Approver
	Own             OwnMessager // nil: no broker exception for commit messages
	RESTURL         string      // default https://api.github.com
	HTTP            *http.Client
	LookupTimeout   time.Duration // server lookup bound; default 5s
}

// object keeps editor and lastEditedAt raw: a missing key (nil) differs from
// an explicit null, and only the explicit form is edit provenance.
type object struct {
	Typename     string          `json:"__typename"`
	ID           string          `json:"id"`
	URL          string          `json:"url"`
	Body         *string         `json:"body"`
	State        string          `json:"state"`
	LastEditedAt json.RawMessage `json:"lastEditedAt"`
	Editor       json.RawMessage `json:"editor"`
}

// provenance reads the edit fields into c. It fails when either key is
// missing or malformed, so an incomplete reply is never taken as "unedited".
func (n *object) provenance(c *Check) error {
	if n.LastEditedAt == nil || n.Editor == nil {
		return errors.New("edit provenance missing")
	}
	var at *string
	var editor *struct {
		DatabaseID *int64 `json:"databaseId"`
	}
	if json.Unmarshal(n.LastEditedAt, &at) != nil || json.Unmarshal(n.Editor, &editor) != nil {
		return errors.New("edit provenance malformed")
	}
	if at != nil {
		c.LastEditedAt = *at
	}
	if editor != nil {
		c.EditorPresent, c.EditorDatabaseID = true, editor.DatabaseID
	}
	return nil
}

// do sends one authenticated GitHub request. The read token goes only to the
// first host: a redirect to another host (log blobs) drops it.
func (p *Proxy) do(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (*http.Response, error) {
	tok, err := p.Token(ctx)
	if err != nil || tok == "" {
		return nil, errors.New("read token unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	base := p.HTTP
	if base == nil {
		base = &http.Client{Timeout: 30 * time.Second}
	}
	client := *base
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if r.URL.Host != via[0].URL.Host {
			r.Header.Del("Authorization")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("github request failed")
	}
	return resp, nil
}

func (p *Proxy) fetch(ctx context.Context, id string) (*object, error) {
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]string{"id": id}})
	endpoint := p.GraphQLURL
	if endpoint == "" {
		endpoint = defaultGraphQL
	}
	resp, err := p.do(ctx, http.MethodPost, endpoint, bytes.NewReader(payload), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Node *object `json:"node"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse || json.Unmarshal(raw, &out) != nil {
		return nil, errors.New("github response unreadable")
	}
	n := out.Data.Node
	if len(out.Errors) > 0 || n == nil || !kinds[n.Typename] || n.Body == nil || n.ID != id {
		return nil, errors.New("object not readable")
	}
	return n, nil
}

func (p *Proxy) read(ctx context.Context, id string) (Result, error) {
	n, err := p.fetch(ctx, id)
	if err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256([]byte(*n.Body))
	res := Result{Kind: n.Typename, ID: n.ID, URL: n.URL, SHA256: hex.EncodeToString(sum[:])}
	c := Check{IssueID: p.IssueID, TaskID: p.TaskID, Kind: n.Typename, ObjectID: n.ID, State: n.State,
		BodySHA256: res.SHA256}
	if err := n.provenance(&c); err != nil {
		return Result{}, err
	}
	if ok, err := p.admit(ctx, func(lctx context.Context) (bool, error) { return p.Admit.Admitted(lctx, c) }); err == nil && ok {
		res.Full, res.Body = true, *n.Body
	}
	return res, nil
}

// admit bounds one server lookup. An error, a timeout, or a success that
// arrives after the deadline is a refusal, so the caller returns a stub.
func (p *Proxy) admit(ctx context.Context, ask func(context.Context) (bool, error)) (bool, error) {
	lookup := p.LookupTimeout
	if lookup <= 0 {
		lookup = defaultLookup
	}
	lctx, cancel := context.WithTimeout(ctx, lookup)
	defer cancel()
	ok, err := ask(lctx)
	return ok && err == nil && lctx.Err() == nil, err
}

// Handler serves GET /v1/object?id=<node id>.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/object", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if !objectID.MatchString(id) {
			http.Error(w, "bad object id", http.StatusBadRequest)
			return
		}
		res, err := p.read(r.Context(), id)
		if err != nil {
			// No upstream bytes in an error.
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
	p.externalRoutes(mux)
	return mux
}

// Serve answers on ln until it is closed.
func (p *Proxy) Serve(ln net.Listener) error {
	return (&http.Server{Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
}

// Read asks the proxy on sock for one object.
func Read(ctx context.Context, sock, id string) (Result, error) {
	return get(ctx, sock, "/v1/object?id="+url.QueryEscape(id))
}

func get(ctx context.Context, sock, pathQuery string) (Result, error) {
	c := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://readproxy"+pathQuery, nil)
	if err != nil {
		return Result{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("read proxy: %s", bytes.TrimSpace(b))
	}
	var res Result
	err = json.Unmarshal(b, &res)
	return res, err
}
