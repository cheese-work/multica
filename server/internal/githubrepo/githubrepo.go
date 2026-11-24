// Package githubrepo creates GitHub repositories with an org-admin credential
// that only the server process holds (CHE-9525). An agent proposes the action;
// the server calls this package after a person approves it.
package githubrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AdminTokensEnv names the server-only variable holding a JSON object that maps
// an org to its admin token. No other process reads it: the agent environment
// builder drops it (pkg/agent.BaseEnviron).
const AdminTokensEnv = "MULTICA_GITHUB_REPO_ADMIN_TOKENS"

// ApprovedOrgs are the orgs an agent may ask to create repositories in.
var ApprovedOrgs = []string{"tevo-studio", "cheese-work"}

// IsApprovedOrg reports whether org is an approved agent target, ignoring case.
func IsApprovedOrg(org string) bool {
	for _, approved := range ApprovedOrgs {
		if strings.EqualFold(approved, org) {
			return true
		}
	}
	return false
}

// Refusals the approval maps to a failed action. Their text never carries a
// credential.
var (
	ErrNotConfigured = errors.New("no admin credential is configured for this org")
	ErrExists        = errors.New("a repository with this name already exists")
	ErrTemplate      = errors.New("the template repository is not available")
)

// Request is one repository to create.
type Request struct {
	Org, Name, Visibility, Template, Description string
}

// Created is the read-back of the new repository.
type Created struct {
	FullName   string
	HTMLURL    string
	Visibility string
}

// Creator creates a repository. The server holds one; tests replace it.
type Creator interface {
	CreateRepo(ctx context.Context, request Request) (Created, error)
}

// Client talks to the GitHub REST API with a per-org admin token.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	tokens  map[string]string
}

// NewClient builds a client from a token map keyed by org (case-insensitive).
func NewClient(baseURL string, tokens map[string]string) *Client {
	lowered := make(map[string]string, len(tokens))
	for org, token := range tokens {
		if strings.TrimSpace(token) != "" {
			lowered[strings.ToLower(org)] = strings.TrimSpace(token)
		}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 20 * time.Second}, tokens: lowered}
}

// NewClientFromEnv reads AdminTokensEnv. An unset or malformed value yields a
// client with no tokens, so every create answers ErrNotConfigured. The error
// never echoes the value.
func NewClientFromEnv(getenv func(string) string) (*Client, error) {
	raw := strings.TrimSpace(getenv(AdminTokensEnv))
	tokens := map[string]string{}
	var err error
	if raw != "" && json.Unmarshal([]byte(raw), &tokens) != nil {
		tokens, err = map[string]string{}, fmt.Errorf("%s is not a JSON object of org to token", AdminTokensEnv)
	}
	return NewClient("https://api.github.com", tokens), err
}

type githubError struct {
	Message string `json:"message"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// CreateRepo creates the repository, from the template when one is named, and
// reads it back.
func (client *Client) CreateRepo(ctx context.Context, request Request) (Created, error) {
	token := client.tokens[strings.ToLower(request.Org)]
	if token == "" {
		return Created{}, ErrNotConfigured
	}
	private := request.Visibility != "public"
	var path string
	var body map[string]any
	if request.Template != "" {
		path = "/repos/" + request.Template + "/generate"
		body = map[string]any{"owner": request.Org, "name": request.Name, "description": request.Description, "private": private}
	} else {
		path = "/orgs/" + url.PathEscape(request.Org) + "/repos"
		body = map[string]any{"name": request.Name, "description": request.Description, "visibility": request.Visibility}
	}
	if err := client.send(ctx, token, http.MethodPost, path, body, request.Template != "", nil); err != nil {
		return Created{}, err
	}
	var repo struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
		Private  bool   `json:"private"`
		Visible  string `json:"visibility"`
	}
	if err := client.send(ctx, token, http.MethodGet, "/repos/"+url.PathEscape(request.Org)+"/"+url.PathEscape(request.Name), nil, false, &repo); err != nil {
		return Created{}, fmt.Errorf("created, but the read-back failed: %w", err)
	}
	visibility := repo.Visible
	if visibility == "" {
		visibility = map[bool]string{true: "private", false: "public"}[repo.Private]
	}
	return Created{FullName: repo.FullName, HTMLURL: repo.HTMLURL, Visibility: visibility}, nil
}

func (client *Client) send(ctx context.Context, token, method, path string, body any, templated bool, into any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, method, client.BaseURL+path, reader)
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("Accept", "application/vnd.github+json")
	httpRequest.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		httpRequest.Header.Set("Content-Type", "application/json")
	}
	response, err := client.HTTP.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("github request failed: %w", scrub(err, token))
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if into != nil {
			return json.Unmarshal(payload, into)
		}
		return nil
	}
	var failure githubError
	_ = json.Unmarshal(payload, &failure)
	if response.StatusCode == http.StatusUnprocessableEntity {
		for _, item := range failure.Errors {
			if strings.Contains(item.Message, "already exists") {
				return ErrExists
			}
		}
	}
	if templated && response.StatusCode == http.StatusNotFound {
		return ErrTemplate
	}
	return fmt.Errorf("github answered %d: %s", response.StatusCode, strings.ReplaceAll(failure.Message, token, "***"))
}

// scrub drops the URL (which could echo a credential in a proxy error) from a
// transport error.
func scrub(err error, token string) error {
	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}
