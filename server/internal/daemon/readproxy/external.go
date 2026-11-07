package readproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// PR titles and bodies and CI logs have no admission path (plan 3.4, CHE-2508).
// They return a stub unless a member approved these exact bytes for the issue
// (a scoped snapshot approval, B3). A CI log of a run on a head the broker
// pushed has no exemption.

// Approval is what the proxy asks the server about one such object.
type Approval struct {
	IssueID, TaskID string
	Kind            string // pr_title, pr_body or ci_log
	ObjectID        string // PR node id, or owner/repo/actions/jobs/<id>
	BodySHA256      string
}

// Approver answers whether a member approved exactly these bytes for the issue.
// A nil Approver approves nothing.
type Approver interface {
	Approved(ctx context.Context, a Approval) (bool, error)
}

const defaultREST = "https://api.github.com"

var (
	repoName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9_-][A-Za-z0-9_.-]{0,99}$`)
	jobID    = regexp.MustCompile(`^[0-9]{1,18}$`)
	commitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

const prQuery = `query($id: ID!) { node(id: $id) { __typename ... on PullRequest { id url title body } } }`

func (p *Proxy) withheld(ctx context.Context, a Approval, text, url string) Result {
	sum := sha256.Sum256([]byte(text))
	a.IssueID, a.TaskID, a.BodySHA256 = p.IssueID, p.TaskID, hex.EncodeToString(sum[:])
	res := Result{Kind: a.Kind, ID: a.ObjectID, URL: url, SHA256: a.BodySHA256}
	if p.Approve == nil {
		return res
	}
	if ok, _ := p.admit(ctx, func(lctx context.Context) (bool, error) { return p.Approve.Approved(lctx, a) }); ok {
		res.Full, res.Body = true, text
	}
	return res
}

func (p *Proxy) readPR(ctx context.Context, id, field string) (Result, error) {
	payload, _ := json.Marshal(map[string]any{"query": prQuery, "variables": map[string]string{"id": id}})
	endpoint := p.GraphQLURL
	if endpoint == "" {
		endpoint = defaultGraphQL
	}
	resp, err := p.do(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)), "application/json")
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("github status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Node *struct {
				Typename string  `json:"__typename"`
				ID       string  `json:"id"`
				URL      string  `json:"url"`
				Title    *string `json:"title"`
				Body     *string `json:"body"`
			} `json:"node"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse || json.Unmarshal(raw, &out) != nil {
		return Result{}, errors.New("github response unreadable")
	}
	n := out.Data.Node
	if len(out.Errors) > 0 || n == nil || n.Typename != "PullRequest" || n.ID != id || n.Title == nil || n.Body == nil {
		return Result{}, errors.New("object not readable")
	}
	text := *n.Title
	if field == "body" {
		text = *n.Body
	}
	return p.withheld(ctx, Approval{Kind: "pr_" + field, ObjectID: id}, text, n.URL), nil
}

func (p *Proxy) readCILog(ctx context.Context, repo, job string) (Result, error) {
	base := p.RESTURL
	if base == "" {
		base = defaultREST
	}
	path := repo + "/actions/jobs/" + job
	resp, err := p.do(ctx, http.MethodGet, base+"/repos/"+path+"/logs", nil, "")
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("github status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse {
		return Result{}, errors.New("log unreadable or too large")
	}
	return p.withheld(ctx, Approval{Kind: "ci_log", ObjectID: path}, string(raw), "https://github.com/"+repo+"/runs/"+job), nil
}

// readCommit returns a commit message as a stub (plan 3.4, CHE-3971). Commit
// messages have no admission path, so only an approval of these exact bytes
// releases them. One exception: a commit the broker pushed for this run, while
// its current bytes still hash to the receipt (Own).
func (p *Proxy) readCommit(ctx context.Context, repo, sha string) (Result, error) {
	base := p.RESTURL
	if base == "" {
		base = defaultREST
	}
	path := repo + "/commits/" + sha
	resp, err := p.do(ctx, http.MethodGet, base+"/repos/"+repo+"/git/commits/"+sha, nil, "")
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("github status %d", resp.StatusCode)
	}
	var out struct {
		SHA     string  `json:"sha"`
		Message *string `json:"message"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse || json.Unmarshal(raw, &out) != nil {
		return Result{}, errors.New("github response unreadable")
	}
	if out.SHA != sha || out.Message == nil {
		return Result{}, errors.New("object not readable")
	}
	link := "https://github.com/" + repo + "/commit/" + sha
	if p.Own != nil && p.Own.OwnMessage(sha, []byte(*out.Message)) {
		sum := sha256.Sum256([]byte(*out.Message))
		return Result{Full: true, Kind: "commit_message", ID: path, URL: link, SHA256: hex.EncodeToString(sum[:]), Body: *out.Message}, nil
	}
	return p.withheld(ctx, Approval{Kind: "commit_message", ObjectID: path}, *out.Message, link), nil
}

func (p *Proxy) externalRoutes(mux *http.ServeMux) {
	serve := func(w http.ResponseWriter, res Result, err error) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway) // no upstream bytes in an error
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
	mux.HandleFunc("GET /v1/pr", func(w http.ResponseWriter, r *http.Request) {
		id, field := r.URL.Query().Get("id"), r.URL.Query().Get("field")
		if !objectID.MatchString(id) || (field != "title" && field != "body") {
			http.Error(w, "bad pull request read", http.StatusBadRequest)
			return
		}
		res, err := p.readPR(r.Context(), id, field)
		serve(w, res, err)
	})
	mux.HandleFunc("GET /v1/ci-log", func(w http.ResponseWriter, r *http.Request) {
		repo, job := r.URL.Query().Get("repo"), r.URL.Query().Get("job")
		if !validRepo(repo) || !jobID.MatchString(job) {
			http.Error(w, "bad log read", http.StatusBadRequest)
			return
		}
		res, err := p.readCILog(r.Context(), repo, job)
		serve(w, res, err)
	})
	mux.HandleFunc("GET /v1/commit", func(w http.ResponseWriter, r *http.Request) {
		repo, sha := r.URL.Query().Get("repo"), r.URL.Query().Get("sha")
		if !validRepo(repo) || !commitID.MatchString(sha) {
			http.Error(w, "bad commit read", http.StatusBadRequest)
			return
		}
		res, err := p.readCommit(r.Context(), repo, sha)
		serve(w, res, err)
	})
}

func validRepo(repo string) bool {
	if !repoName.MatchString(repo) {
		return false
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// ReadPR asks the proxy on sock for a pull request's title or body.
func ReadPR(ctx context.Context, sock, id, field string) (Result, error) {
	return get(ctx, sock, "/v1/pr?id="+url.QueryEscape(id)+"&field="+url.QueryEscape(field))
}

// ReadCILog asks the proxy on sock for one job's log.
func ReadCILog(ctx context.Context, sock, repo, job string) (Result, error) {
	return get(ctx, sock, "/v1/ci-log?repo="+url.QueryEscape(repo)+"&job="+url.QueryEscape(job))
}

// ReadCommit asks the proxy on sock for one commit's message.
func ReadCommit(ctx context.Context, sock, repo, sha string) (Result, error) {
	return get(ctx, sock, "/v1/commit?repo="+url.QueryEscape(repo)+"&sha="+url.QueryEscape(sha))
}
