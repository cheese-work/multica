package taskgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

var ErrUnavailable = errors.New("trusted task gateway unavailable")

const Capability = "task-gateway-v1"

type Policy struct {
	RuntimeID      string           `json:"runtime_id"`
	OwnerID        string           `json:"owner_id"`
	WorkspaceID    string           `json:"workspace_id"`
	TaskID         string           `json:"task_id"`
	GatewayOwnerID int64            `json:"gateway_owner_id"`
	Limit          int64            `json:"limit_tenths"`
	KeyIDs         map[string]int64 `json:"key_ids"`
}

type Provisioner struct {
	baseURL  string
	token    string
	policies map[string]Policy
	client   *http.Client
}

type Grant struct {
	Binding credentialexec.Binding `json:"binding"`
	BaseURL string                 `json:"base_url"`
	Key     string                 `json:"-"`
}

func (grant Grant) Format(writer fmt.State, verb rune) {
	_, _ = io.WriteString(writer, "<task-bound gateway credential>")
}
func (operator Provisioner) Format(writer fmt.State, verb rune) {
	_, _ = io.WriteString(writer, "<trusted task gateway operator>")
}

func New(baseURL, token string, policies []Policy) (*Provisioner, error) {
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Opaque != "" || endpoint.Path != "" && endpoint.Path != "/" || endpoint.Scheme != "https" && endpoint.Scheme != "http" || token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return nil, ErrUnavailable
	}
	if endpoint.Scheme == "http" {
		address := net.ParseIP(endpoint.Hostname())
		if address == nil || !address.IsLoopback() {
			return nil, ErrUnavailable
		}
	}
	operator := &Provisioner{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, policies: make(map[string]Policy), client: &http.Client{
		Timeout:       20 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	for _, policy := range policies {
		binding := credentialexec.Binding{TaskID: policy.TaskID, OwnerID: policy.OwnerID, WorkspaceID: policy.WorkspaceID}
		runtime, runtimeErr := uuid.Parse(policy.RuntimeID)
		if binding.Validate() != nil || runtimeErr != nil || runtime == uuid.Nil || runtime.String() != policy.RuntimeID || policy.GatewayOwnerID <= 0 || policy.Limit <= 0 || policy.KeyIDs["claude"] <= 0 || policy.KeyIDs["codex"] <= 0 || len(policy.KeyIDs) != 2 {
			return nil, ErrUnavailable
		}
		if _, duplicate := operator.policies[policy.RuntimeID]; duplicate {
			return nil, ErrUnavailable
		}
		for _, existing := range operator.policies {
			if existing.GatewayOwnerID == policy.GatewayOwnerID && existing.OwnerID != policy.OwnerID || existing.TaskID == policy.TaskID {
				return nil, ErrUnavailable
			}
		}
		policy.KeyIDs = maps.Clone(policy.KeyIDs)
		operator.policies[policy.RuntimeID] = policy
	}
	return operator, nil
}

func (operator *Provisioner) Requires(runtimeID string) bool {
	if operator == nil {
		return false
	}
	_, present := operator.policies[runtimeID]
	return present
}

func (operator *Provisioner) Authorize(runtimeID, provider string, binding credentialexec.Binding) (Policy, error) {
	if operator == nil || binding.Validate() != nil {
		return Policy{}, ErrUnavailable
	}
	policy, present := operator.policies[runtimeID]
	if !present || policy.OwnerID != binding.OwnerID || policy.WorkspaceID != binding.WorkspaceID || policy.TaskID != binding.TaskID || provider != "claude" && provider != "codex" {
		return Policy{}, ErrUnavailable
	}
	policy.KeyIDs = maps.Clone(policy.KeyIDs)
	return policy, nil
}

func (operator *Provisioner) Provision(ctx context.Context, runtimeID, provider string, binding credentialexec.Binding) (Grant, error) {
	policy, err := operator.Authorize(runtimeID, provider, binding)
	if err != nil {
		return Grant{}, err
	}
	var keys struct {
		Items []struct {
			ID      int64      `json:"id"`
			Owner   int64      `json:"user_id"`
			Key     string     `json:"key"`
			Status  string     `json:"status"`
			Expires *time.Time `json:"expires_at"`
		} `json:"items"`
		Pages int `json:"pages"`
	}
	if err := operator.request(ctx, http.MethodGet, fmt.Sprintf("/api/v1/admin/users/%d/api-keys?page_size=100", policy.GatewayOwnerID), nil, &keys); err != nil {
		return Grant{}, err
	}
	if keys.Pages != 1 {
		return Grant{}, ErrUnavailable
	}
	keyID := policy.KeyIDs[provider]
	selected := make(map[int64]string)
	for _, candidate := range keys.Items {
		if candidate.ID == policy.KeyIDs["claude"] || candidate.ID == policy.KeyIDs["codex"] {
			if selected[candidate.ID] != "" || candidate.Owner != policy.GatewayOwnerID || candidate.Status != "active" || candidate.Key == "" || candidate.Key == operator.token || strings.ContainsAny(candidate.Key, "\r\n\x00") || candidate.Expires != nil && !candidate.Expires.After(time.Now()) {
				return Grant{}, ErrUnavailable
			}
			selected[candidate.ID] = candidate.Key
		}
	}
	if selected[policy.KeyIDs["claude"]] == "" || selected[policy.KeyIDs["codex"]] == "" {
		return Grant{}, ErrUnavailable
	}
	key := selected[keyID]
	keyIDs := []int64{policy.KeyIDs["claude"]}
	if policy.KeyIDs["codex"] != keyIDs[0] {
		keyIDs = append(keyIDs, policy.KeyIDs["codex"])
	}
	body := struct {
		Owner int64   `json:"owner_id"`
		Keys  []int64 `json:"api_key_ids"`
		Limit int64   `json:"limit_tenths"`
	}{policy.GatewayOwnerID, keyIDs, policy.Limit}
	path := "/api/v1/admin/task-quotas/" + binding.TaskID
	var provisioned struct {
		TaskID string `json:"task_id"`
	}
	if err := operator.request(ctx, http.MethodPut, path, body, &provisioned); err != nil || provisioned.TaskID != binding.TaskID {
		return Grant{}, ErrUnavailable
	}
	var snapshot struct {
		TaskID         string `json:"task_id"`
		Closed         *bool  `json:"closed"`
		Reconciliation *bool  `json:"reconciliation_required"`
		Policy         struct {
			Version  int    `json:"version"`
			Unit     string `json:"unit"`
			Input    int    `json:"input_weight"`
			Creation int    `json:"cache_creation_weight"`
			Read     int    `json:"cache_read_weight"`
			Output   int    `json:"output_weight"`
		} `json:"policy"`
		Exact struct {
			Owner     string   `json:"owner_id"`
			Keys      []string `json:"api_key_ids"`
			Limit     string   `json:"limit_tenths"`
			Spent     string   `json:"spent_tenths"`
			Held      string   `json:"held_tenths"`
			Remaining string   `json:"remaining_tenths"`
		} `json:"exact"`
	}
	if err := operator.request(ctx, http.MethodGet, path, nil, &snapshot); err != nil {
		return Grant{}, err
	}
	limit, limitErr := strconv.ParseInt(snapshot.Exact.Limit, 10, 64)
	spent, spentErr := strconv.ParseInt(snapshot.Exact.Spent, 10, 64)
	held, heldErr := strconv.ParseInt(snapshot.Exact.Held, 10, 64)
	remaining, remainingErr := strconv.ParseInt(snapshot.Exact.Remaining, 10, 64)
	if snapshot.TaskID != binding.TaskID || snapshot.Exact.Owner != strconv.FormatInt(policy.GatewayOwnerID, 10) || limitErr != nil || spentErr != nil || heldErr != nil || remainingErr != nil || limit != policy.Limit || spent < 0 || held < 0 || spent >= limit || held >= limit-spent || remaining != limit-spent-held || snapshot.Closed == nil || *snapshot.Closed || snapshot.Reconciliation == nil || *snapshot.Reconciliation || snapshot.Policy.Version != 1 || snapshot.Policy.Unit != "weighted_token_tenths" || snapshot.Policy.Input != 10 || snapshot.Policy.Creation != 10 || snapshot.Policy.Read != 1 || snapshot.Policy.Output != 50 || len(snapshot.Exact.Keys) != len(keyIDs) {
		return Grant{}, ErrUnavailable
	}
	for _, exact := range []struct {
		text  string
		value int64
	}{{snapshot.Exact.Limit, limit}, {snapshot.Exact.Spent, spent}, {snapshot.Exact.Held, held}, {snapshot.Exact.Remaining, remaining}} {
		if exact.text != strconv.FormatInt(exact.value, 10) {
			return Grant{}, ErrUnavailable
		}
	}
	bound := make(map[string]bool)
	for _, boundID := range snapshot.Exact.Keys {
		if bound[boundID] {
			return Grant{}, ErrUnavailable
		}
		bound[boundID] = true
	}
	for _, requiredID := range keyIDs {
		if !bound[strconv.FormatInt(requiredID, 10)] {
			return Grant{}, ErrUnavailable
		}
	}
	return Grant{Binding: binding, BaseURL: operator.baseURL, Key: key}, nil
}

func (operator *Provisioner) request(ctx context.Context, method, path string, body, target any) error {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return ErrUnavailable
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, operator.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return ErrUnavailable
	}
	request.Header.Set("X-Api-Key", operator.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := operator.client.Do(request)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	const responseLimit = 1 << 20
	contents, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil || len(contents) > responseLimit {
		return ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if validateJSONFields(decoder, 0) != nil {
		return ErrUnavailable
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrUnavailable
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(contents, &envelope) != nil || envelope.Code == nil || *envelope.Code != 0 || len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, target) != nil {
		return ErrUnavailable
	}
	return nil
}

func validateJSONFields(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return ErrUnavailable
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrUnavailable
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		return nil
	}
	fields := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			field, err := decoder.Token()
			name, valid := field.(string)
			if err != nil || !valid {
				return ErrUnavailable
			}
			name = strings.Map(func(character rune) rune {
				for {
					folded := unicode.SimpleFold(character)
					if folded <= character {
						return folded
					}
					character = folded
				}
			}, name)
			if fields[name] {
				return ErrUnavailable
			}
			fields[name] = true
		}
		if validateJSONFields(decoder, depth+1) != nil {
			return ErrUnavailable
		}
	}
	_, err = decoder.Token()
	if err != nil {
		return ErrUnavailable
	}
	return nil
}
