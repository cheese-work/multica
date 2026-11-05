package ghsnapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Typed stacks-API failures (CHE-4143 step 2). Callers branch with errors.Is;
// rate limiting stays *RateLimitError. None of them carries a body or token.
var (
	// ErrStacksUnsupported: 404 — the repo or the preview stacks API is absent.
	ErrStacksUnsupported = errors.New("github stacks: unsupported")
	// ErrStacksNoPermission: 403 — the App lacks the permission this call needs.
	ErrStacksNoPermission = errors.New("github stacks: no permission")
	// ErrStacksConflict: 422 — the stack changed or the PRs cannot be stacked.
	ErrStacksConflict = errors.New("github stacks: conflict")
	// ErrStacksSchema: the response is not the shape observed 5 Nov 2026
	// (unknown or missing fields). The API is a preview; treat as a report.
	ErrStacksSchema = errors.New("github stacks: unexpected response shape")
)

// Stack is one GitHub stack. PullRequests is ordered bottom first.
type Stack struct {
	Number       int
	BaseRef      string
	Open         bool
	PullRequests []StackPR
}

type StackPR struct {
	Number   int
	State    string
	Draft    bool
	MergedAt string // RFC 3339, empty when not merged
	HeadRef  string
	HeadSHA  string
}

// Wire shape. Unknown fields are rejected (DisallowUnknownFields); id,
// node_id, url and created_at are known but unused.
type stackWire struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id"`
	URL       string `json:"url"`
	CreatedAt string `json:"created_at"`
	Number    int    `json:"number"`
	Base      struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Open         *bool         `json:"open"` // nil = missing or null: schema error
	PullRequests []stackPRWire `json:"pull_requests"`
}

type stackPRWire struct {
	Number   int             `json:"number"`
	State    string          `json:"state"`
	Draft    *bool           `json:"draft"`     // nil = missing or null: schema error
	MergedAt json.RawMessage `json:"merged_at"` // nil = missing: schema error; `null` = not merged
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return ErrStacksSchema
	}
	if dec.More() {
		return ErrStacksSchema
	}
	return nil
}

func (w stackWire) stack() (Stack, error) {
	if w.Number <= 0 || w.Base.Ref == "" || w.Open == nil || w.PullRequests == nil {
		return Stack{}, ErrStacksSchema
	}
	s := Stack{Number: w.Number, BaseRef: w.Base.Ref, Open: *w.Open, PullRequests: make([]StackPR, 0, len(w.PullRequests))}
	for _, p := range w.PullRequests {
		if p.Number <= 0 || p.State == "" || p.Draft == nil || p.MergedAt == nil || p.Head.Ref == "" || p.Head.SHA == "" {
			return Stack{}, ErrStacksSchema
		}
		pr := StackPR{Number: p.Number, State: p.State, Draft: *p.Draft, HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA}
		if string(p.MergedAt) != "null" {
			if err := json.Unmarshal(p.MergedAt, &pr.MergedAt); err != nil {
				return Stack{}, ErrStacksSchema
			}
		}
		s.PullRequests = append(s.PullRequests, pr)
	}
	return s, nil
}

func parseStack(raw []byte) (Stack, error) {
	var w stackWire
	if err := decodeStrict(raw, &w); err != nil {
		return Stack{}, err
	}
	return w.stack()
}

// mapStacksErr turns the transport's StatusError into a typed stacks error.
// Rate limits and other errors pass through unchanged.
func mapStacksErr(err error) error {
	var se *StatusError
	if !errors.As(err, &se) {
		return err
	}
	switch se.Status {
	case http.StatusNotFound:
		return ErrStacksUnsupported
	case http.StatusForbidden:
		return ErrStacksNoPermission
	case http.StatusUnprocessableEntity:
		return ErrStacksConflict
	}
	return err
}

func (*Client) stacksPath(owner, repo string) string {
	return fmt.Sprintf("/repos/%s/%s/stacks", url.PathEscape(owner), url.PathEscape(repo))
}

var errStacksNotConfigured = errors.New("github: stacks API is not configured")

// StacksForPR lists the stacks that contain pull request pr (empty when none).
func (client *Client) StacksForPR(ctx context.Context, installationID int64, owner, repo string, pr int) ([]Stack, error) {
	if !client.Enabled() {
		return nil, errStacksNotConfigured
	}
	raw, err := client.restGet(ctx, installationID, fmt.Sprintf("%s?pull_request=%d", client.stacksPath(owner, repo), pr))
	if err != nil {
		return nil, mapStacksErr(err)
	}
	var wires []stackWire
	if err := decodeStrict(raw, &wires); err != nil || wires == nil {
		return nil, ErrStacksSchema
	}
	stacks := make([]Stack, 0, len(wires))
	for _, w := range wires {
		s, err := w.stack()
		if err != nil {
			return nil, err
		}
		stacks = append(stacks, s)
	}
	return stacks, nil
}

// GetStack reads stack number n.
func (client *Client) GetStack(ctx context.Context, installationID int64, owner, repo string, n int) (Stack, error) {
	if !client.Enabled() {
		return Stack{}, errStacksNotConfigured
	}
	raw, err := client.restGet(ctx, installationID, fmt.Sprintf("%s/%d", client.stacksPath(owner, repo), n))
	if err != nil {
		return Stack{}, mapStacksErr(err)
	}
	return parseStack(raw)
}

type stackPRsPayload struct {
	PullRequests []int `json:"pull_requests"`
}

func validPRs(prs []int) bool {
	seen := map[int]bool{}
	for _, n := range prs {
		if n <= 0 || seen[n] {
			return false
		}
		seen[n] = true
	}
	return len(prs) > 0
}

// CreateStack creates a stack from prs, bottom first. The payload field name
// `pull_requests` (PR numbers) is inferred from the gh-stack v0.1.1 client and
// the read shape; the CHE-4143 step 1 write probe confirms it.
func (client *Client) CreateStack(ctx context.Context, installationID int64, owner, repo string, prs []int) (Stack, error) {
	if !client.Enabled() {
		return Stack{}, errStacksNotConfigured
	}
	if !validPRs(prs) {
		return Stack{}, errors.New("github stacks: pull request list must be non-empty, positive and unique")
	}
	payload, _ := json.Marshal(stackPRsPayload{prs})
	raw, err := client.restDo(ctx, installationID, http.MethodPost, client.stacksPath(owner, repo), payload, http.StatusCreated, http.StatusOK)
	if err != nil {
		return Stack{}, mapStacksErr(err)
	}
	return parseStack(raw)
}

// AddToStack appends prs, bottom first, to stack n.
func (client *Client) AddToStack(ctx context.Context, installationID int64, owner, repo string, n int, prs []int) (Stack, error) {
	if !client.Enabled() {
		return Stack{}, errStacksNotConfigured
	}
	if !validPRs(prs) {
		return Stack{}, errors.New("github stacks: pull request list must be non-empty, positive and unique")
	}
	payload, _ := json.Marshal(stackPRsPayload{prs})
	raw, err := client.restDo(ctx, installationID, http.MethodPost, fmt.Sprintf("%s/%d/add", client.stacksPath(owner, repo), n), payload, http.StatusCreated, http.StatusOK)
	if err != nil {
		return Stack{}, mapStacksErr(err)
	}
	return parseStack(raw)
}
