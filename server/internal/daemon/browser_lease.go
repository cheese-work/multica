package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
)

// Browser lease proxy (CHE-4743). The agent holds a mat_ task token; the lease routes accept only
// the task's mdt_ daemon credential, which stays here. The task id comes from the bound task,
// never from the request. A body or token is never logged.

var browserLeaseIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

type browserLeaseRequest struct {
	Op         string `json:"op"`
	ProfileID  string `json:"profile_id"`
	DecisionID string `json:"decision_id"`
	LeaseID    string `json:"lease_id"`
}

// browserLeaseCall maps a request to the server route; ok is false for a malformed request.
func browserLeaseCall(taskID string, req browserLeaseRequest) (method, path string, body any, ok bool) {
	base := "/api/daemon/tasks/" + url.PathEscape(taskID) + "/browser-leases"
	if req.Op == "lease" {
		if req.ProfileID == "" {
			return
		}
		body = map[string]string{"profile_id": req.ProfileID}
		if req.DecisionID != "" {
			body = map[string]string{"profile_id": req.ProfileID, "decision_id": req.DecisionID}
		}
		return http.MethodPost, base, body, true
	}
	if !browserLeaseIDPattern.MatchString(req.LeaseID) {
		return
	}
	switch req.Op {
	case "renew":
		return http.MethodPost, base + "/" + req.LeaseID + "/renew", nil, true
	case "release":
		return http.MethodPost, base + "/" + req.LeaseID + "/release", nil, true
	case "status":
		return http.MethodGet, base + "/" + req.LeaseID, nil, true
	}
	return
}

func (d *Daemon) browserLeaseHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		task, result := d.activeRepoCheckoutTask(r)
		if result != repoCheckoutAuthOK {
			http.Error(w, "browser lease needs the credential of a task running in this daemon", http.StatusUnauthorized)
			return
		}
		if task.DaemonToken == "" {
			http.Error(w, "this task has no daemon credential for browser leases", http.StatusServiceUnavailable)
			return
		}
		var req browserLeaseRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		method, path, body, ok := browserLeaseCall(task.TaskID, req)
		if !ok {
			http.Error(w, "invalid browser lease request: op is lease|renew|release|status; lease needs profile_id, the rest need lease_id", http.StatusBadRequest)
			return
		}
		status, reply, err := d.client.doRaw(r.Context(), method, path, task.DaemonToken, body)
		if err != nil {
			d.logger.Warn("browser lease call failed", "task_id", task.TaskID, "op", req.Op, "error", err)
			http.Error(w, "browser lease call failed", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(reply)
	}
}

// doRaw sends one call with an explicit credential and returns the status and body as they are.
func (c *Client) doRaw(ctx context.Context, method, path, token string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	c.setIdentityHeaders(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, data, err
}
