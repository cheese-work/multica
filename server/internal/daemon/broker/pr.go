package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PRRequest is one own-PR operation (plan 3.2, broker item 6). There is no
// head field: the head is always the run's task branch. There is no merge,
// review or settings operation to ask for.
type PRRequest struct {
	Op     string  `json:"op"` // create, edit, ready, comment
	Number int     `json:"number,omitempty"`
	Title  *string `json:"title,omitempty"`
	Body   *string `json:"body,omitempty"`
	Base   string  `json:"base,omitempty"` // create only
	Draft  bool    `json:"draft,omitempty"`
}

// PRResult names the PR an operation touched.
type PRResult struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type ghPR struct {
	Number  int    `json:"number"`
	NodeID  string `json:"node_id"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref  string `json:"ref"`
		Repo struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"head"`
}

func (b *Broker) handlePR(w http.ResponseWriter, r *http.Request) {
	if b.pin.Tokens == nil {
		http.Error(w, "this run has no GitHub access", http.StatusNotImplemented)
		return
	}
	var req PRRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	began := time.Now()
	res, status, err := b.prOp(r.Context(), req)
	b.log.Info("broker pr", "repo", b.pin.Repo, "op", req.Op, "number", res.Number, "status", status, "elapsed", time.Since(began), "error", err)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// check refuses what the broker will never do, before any token exists.
func (b *Broker) check(req PRRequest) (int, error) {
	bad := func(format string, a ...any) (int, error) { return http.StatusBadRequest, fmt.Errorf(format, a...) }
	if req.Op != "create" && (req.Base != "" || req.Draft) {
		return bad("base and draft apply to create only")
	}
	switch req.Op {
	case "create":
		if req.Title == nil || *req.Title == "" {
			return bad("create needs a title")
		}
		if req.Number != 0 {
			return bad("create takes no number")
		}
		for _, base := range b.pin.Bases {
			if req.Base == base {
				return 0, nil
			}
		}
		return http.StatusForbidden, fmt.Errorf("base %q is not one of %v", req.Base, b.pin.Bases)
	case "edit", "ready", "comment":
		if req.Number <= 0 {
			return bad("%s needs a PR number", req.Op)
		}
		switch {
		case req.Op == "edit" && req.Title == nil && req.Body == nil:
			return bad("edit needs a title or a body")
		case req.Op == "comment" && (req.Body == nil || *req.Body == ""):
			return bad("comment needs a body")
		case req.Op != "edit" && req.Title != nil, req.Op == "ready" && req.Body != nil:
			return bad("%s takes no title or body of that kind", req.Op)
		}
		return 0, nil
	}
	return bad("unknown op %q: only create, edit, ready and comment exist", req.Op)
}

func (b *Broker) prOp(parent context.Context, req PRRequest) (res PRResult, status int, err error) {
	if status, err = b.check(req); err != nil {
		return res, status, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	in, _ := json.Marshal(req)
	op, status, err := b.authorize(parent, inputHash("pr", b.pin.Repo, b.pin.Ref, string(in)))
	if err != nil {
		return res, status, err
	}
	defer func() { b.finish(op, err) }()
	g, status, err := b.mint(parent, true)
	if err != nil {
		return res, status, err
	}
	defer b.revoke(g)
	ctx, cancel := context.WithTimeout(parent, b.pin.deadline())
	defer cancel()

	repo := "/repos/" + b.pin.Slug
	var pr ghPR
	if req.Op == "create" {
		if err := b.write(op); err != nil {
			return res, http.StatusForbidden, err
		}
		in := map[string]any{"title": *req.Title, "head": strings.TrimPrefix(b.pin.Ref, "refs/heads/"), "base": req.Base, "draft": req.Draft}
		if req.Body != nil {
			in["body"] = *req.Body
		}
		if status, err = b.gh(ctx, g, http.MethodPost, repo+"/pulls", in, &pr); err != nil {
			return res, status, err
		}
		return PRResult{pr.Number, pr.HTMLURL}, http.StatusOK, nil
	}

	// Anything but create must prove the PR is this run's own: its head is the
	// task branch of the task repository.
	pulls := fmt.Sprintf("%s/pulls/%d", repo, req.Number)
	if status, err = b.gh(ctx, g, http.MethodGet, pulls, nil, &pr); err != nil {
		return res, status, err
	}
	if id, _ := b.pin.repoID(); pr.Head.Ref != strings.TrimPrefix(b.pin.Ref, "refs/heads/") || pr.Head.Repo.ID != id {
		return res, http.StatusForbidden, errors.New("that PR is not this run's own")
	}
	res = PRResult{pr.Number, pr.HTMLURL}
	if err := b.write(op); err != nil {
		return res, http.StatusForbidden, err
	}
	switch req.Op {
	case "edit":
		in := map[string]any{}
		if req.Title != nil {
			in["title"] = *req.Title
		}
		if req.Body != nil {
			in["body"] = *req.Body
		}
		status, err = b.gh(ctx, g, http.MethodPatch, pulls, in, nil)
	case "comment":
		status, err = b.gh(ctx, g, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", repo, req.Number), map[string]any{"body": *req.Body}, nil)
	case "ready": // REST has no endpoint for this; GraphQL does
		var out struct {
			Data   json.RawMessage            `json:"data"`
			Errors []struct{ Message string } `json:"errors"`
		}
		status, err = b.gh(ctx, g, http.MethodPost, "/graphql", map[string]any{
			"query":     "mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { id } } }",
			"variables": map[string]string{"id": pr.NodeID},
		}, &out)
		if err == nil {
			status, err = http.StatusBadGateway, readyOutcome(out.Data, len(out.Errors) > 0)
		}
	}
	if err != nil {
		return res, status, err
	}
	return res, http.StatusOK, nil
}

// readyOutcome reads a GraphQL answer to the ready mutation. GraphQL answers 200
// for every outcome, so only two are definite: the success payload with no
// error, and an error with no data entry at all, which GraphQL raises before it
// executes anything. An error next to a data entry, or no payload, may have
// followed a mutation that ran.
func readyOutcome(data json.RawMessage, hasErrors bool) error {
	var d struct {
		Mark *struct {
			PullRequest *struct{ ID string } `json:"pullRequest"`
		} `json:"markPullRequestReadyForReview"`
	}
	_ = json.Unmarshal(data, &d)
	switch {
	case !hasErrors && d.Mark != nil && d.Mark.PullRequest != nil && d.Mark.PullRequest.ID != "":
		return nil
	case hasErrors && len(data) == 0:
		return notApplied{errors.New("github: graphql request failed before execution")}
	}
	return errors.New("github: graphql answer does not show whether the PR was marked ready")
}

// gh makes one authorized call and returns the HTTP status to answer with if
// it failed. GitHub's own error message is passed on; it never holds a token.
func (b *Broker) gh(ctx context.Context, g grant, method, path string, in, out any) (int, error) {
	resp, err := b.pin.Tokens.call(ctx, method, path, g.token, in)
	if err != nil {
		return http.StatusBadGateway, fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var msg struct{ Message string }
		_ = json.Unmarshal(body, &msg)
		err := fmt.Errorf("github %s %s: status %d: %.200s", method, path, resp.StatusCode, msg.Message)
		if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusRequestTimeout { // GitHub refused it
			err = notApplied{err}
		}
		return http.StatusBadGateway, err
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return http.StatusBadGateway, fmt.Errorf("github %s %s: malformed response", method, path)
		}
	}
	return 0, nil
}
