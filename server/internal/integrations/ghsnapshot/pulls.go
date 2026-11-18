package ghsnapshot

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PullRef is the part of a pull request the stack linker reads (CHE-4143
// step 6). State is "open" or "closed"; Merged tells a merged PR apart.
type PullRef struct {
	Number     int
	Head, Base string
	State      string
	Merged     bool
	Fork       bool // head branch lives in another repo
}

// NewClientWithAPIBase builds a Client against apiBase. Tests of packages that
// use the Client point it at an httptest server.
func NewClientWithAPIBase(appID string, key *rsa.PrivateKey, apiBase string) *Client {
	return &Client{
		appID:      appID,
		privateKey: key,
		apiBase:    apiBase,
		httpClient: &http.Client{Timeout: 20 * time.Second},
		now:        time.Now,
		tokens:     map[int64]cachedToken{},
	}
}

var errPullShape = errors.New("github pulls: unexpected response shape")

type pullWire struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (w pullWire) ref(owner, repo string) (PullRef, error) {
	if w.Number <= 0 || w.State == "" || w.Head.Ref == "" || w.Base.Ref == "" {
		return PullRef{}, errPullShape
	}
	// A deleted fork has a null head repo; treat it as a fork, never as same-repo.
	fork := w.Head.Repo == nil || !strings.EqualFold(w.Head.Repo.FullName, owner+"/"+repo)
	return PullRef{Number: w.Number, Head: w.Head.Ref, Base: w.Base.Ref, State: w.State, Merged: w.Merged, Fork: fork}, nil
}

func repoPath(owner, repo string) string {
	return fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
}

// DefaultBranch reads the repo's default branch.
func (client *Client) DefaultBranch(ctx context.Context, installationID int64, owner, repo string) (string, error) {
	if !client.Enabled() {
		return "", errStacksNotConfigured
	}
	raw, err := client.restGet(ctx, installationID, repoPath(owner, repo))
	if err != nil {
		return "", err
	}
	var w struct {
		DefaultBranch string `json:"default_branch"`
	}
	if json.Unmarshal(raw, &w) != nil || w.DefaultBranch == "" {
		return "", errPullShape
	}
	return w.DefaultBranch, nil
}

// PullByNumber reads one pull request.
func (client *Client) PullByNumber(ctx context.Context, installationID int64, owner, repo string, n int) (PullRef, error) {
	if !client.Enabled() {
		return PullRef{}, errStacksNotConfigured
	}
	raw, err := client.restGet(ctx, installationID, fmt.Sprintf("%s/pulls/%d", repoPath(owner, repo), n))
	if err != nil {
		return PullRef{}, err
	}
	var w pullWire
	if json.Unmarshal(raw, &w) != nil {
		return PullRef{}, errPullShape
	}
	return w.ref(owner, repo)
}

// PullsByHead lists every pull request (any state) whose head branch is head
// in the same repo. Fork heads are excluded by the owner-qualified filter. One
// page of 100: more than 100 PRs for one branch name is ambiguous anyway.
func (client *Client) PullsByHead(ctx context.Context, installationID int64, owner, repo, head string) ([]PullRef, error) {
	if !client.Enabled() {
		return nil, errStacksNotConfigured
	}
	q := url.Values{"head": {owner + ":" + head}, "state": {"all"}, "per_page": {"100"}}
	raw, err := client.restGet(ctx, installationID, fmt.Sprintf("%s/pulls?%s", repoPath(owner, repo), q.Encode()))
	if err != nil {
		return nil, err
	}
	var wires []pullWire
	if json.Unmarshal(raw, &wires) != nil || wires == nil {
		return nil, errPullShape
	}
	out := make([]PullRef, 0, len(wires))
	for _, w := range wires {
		p, err := w.ref(owner, repo)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
