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

// ponytail: comments, reviews and review comments only; PR title and body,
// commit message and CI log stubs arrive with B2c.
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
	HTTP            *http.Client
	LookupTimeout   time.Duration // server lookup bound; default 5s
}

type object struct {
	Typename     string  `json:"__typename"`
	ID           string  `json:"id"`
	URL          string  `json:"url"`
	Body         *string `json:"body"`
	State        string  `json:"state"`
	LastEditedAt *string `json:"lastEditedAt"`
	Editor       *struct {
		DatabaseID *int64 `json:"databaseId"`
	} `json:"editor"`
}

func (p *Proxy) fetch(ctx context.Context, id string) (*object, error) {
	tok, err := p.Token(ctx)
	if err != nil || tok == "" {
		return nil, errors.New("read token unavailable")
	}
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]string{"id": id}})
	endpoint := p.GraphQLURL
	if endpoint == "" {
		endpoint = defaultGraphQL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("github request failed")
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out); err != nil {
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
		BodySHA256: res.SHA256, EditorPresent: n.Editor != nil}
	if n.LastEditedAt != nil {
		c.LastEditedAt = *n.LastEditedAt
	}
	if n.Editor != nil {
		c.EditorDatabaseID = n.Editor.DatabaseID
	}
	lookup := p.LookupTimeout
	if lookup <= 0 {
		lookup = defaultLookup
	}
	lctx, cancel := context.WithTimeout(ctx, lookup)
	defer cancel()
	// Lookup error or timeout is a stub, never full text.
	if ok, err := p.Admit.Admitted(lctx, c); err == nil && ok {
		res.Full, res.Body = true, *n.Body
	}
	return res, nil
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
	return mux
}

// Serve answers on ln until it is closed.
func (p *Proxy) Serve(ln net.Listener) error {
	return (&http.Server{Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
}

// Read asks the proxy on sock for one object.
func Read(ctx context.Context, sock, id string) (Result, error) {
	c := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://readproxy/v1/object?id="+url.QueryEscape(id), nil)
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
	return res, json.Unmarshal(b, &res)
}
