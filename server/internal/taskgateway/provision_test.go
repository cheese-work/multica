package taskgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

func gatewayFixture(test *testing.T) (*Provisioner, credentialexec.Binding, *atomic.Int32) {
	return gatewayFixtureResponse(test, nil)
}

func gatewayFixtureResponse(test *testing.T, change func(string, string) string) (*Provisioner, credentialexec.Binding, *atomic.Int32) {
	test.Helper()
	binding := credentialexec.Binding{TaskID: "00000000-0000-4000-8000-000000000001", OwnerID: "00000000-0000-4000-8000-000000000002", WorkspaceID: "00000000-0000-4000-8000-000000000003"}
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Header.Get("X-Api-Key") != "owned-operator-secret" {
			test.Errorf("operator credential missing")
		}
		var response string
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/admin/users/7/api-keys":
			response = `{"code":0,"data":{"items":[{"id":11,"user_id":7,"key":"owned-task-secret","status":"active"}],"pages":1}}`
		case "PUT /api/v1/admin/task-quotas/" + binding.TaskID:
			var body struct {
				Owner int64   `json:"owner_id"`
				Keys  []int64 `json:"api_key_ids"`
				Limit int64   `json:"limit_tenths"`
			}
			if json.NewDecoder(request.Body).Decode(&body) != nil || body.Owner != 7 || len(body.Keys) != 1 || body.Keys[0] != 11 || body.Limit != 100 {
				test.Error("wrong provisioning contract")
			}
			response = `{"code":0,"data":{"task_id":"` + binding.TaskID + `"}}`
		case "GET /api/v1/admin/task-quotas/" + binding.TaskID:
			response = `{"code":0,"data":{"task_id":"` + binding.TaskID + `","closed":false,"reconciliation_required":false,"policy":{"version":1,"unit":"weighted_token_tenths","input_weight":10,"cache_creation_weight":10,"cache_read_weight":1,"output_weight":50},"exact":{"owner_id":"7","api_key_ids":["11"],"limit_tenths":"100","spent_tenths":"20","held_tenths":"0","remaining_tenths":"80"}}}`
		default:
			test.Errorf("unexpected operator route: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
		if change != nil {
			response = change(request.Method+" "+request.URL.Path, response)
		}
		fmt.Fprint(writer, response)
	}))
	test.Cleanup(server.Close)
	operator, err := New(server.URL, "owned-operator-secret", []Policy{{RuntimeID: "00000000-0000-4000-8000-000000000004", OwnerID: binding.OwnerID, WorkspaceID: binding.WorkspaceID, TaskID: binding.TaskID, GatewayOwnerID: 7, Limit: 100, KeyIDs: map[string]int64{"claude": 11, "codex": 11}}})
	if err != nil {
		test.Fatal(err)
	}
	return operator, binding, calls
}

func TestTaskGatewayRejectsUnsafeSnapshot(test *testing.T) {
	for _, replacement := range []struct{ name, from, to string }{
		{"missing closed", `"closed":false,`, ``},
		{"duplicate closed", `"closed":false`, `"closed":true,"closed":false`},
		{"case-folded closed", `"closed":false`, `"Closed":true,"closed":false`},
		{"escaped closed", `"closed":false`, `"\u0063losed":true,"closed":false`},
		{"unicode folded counter", `"spent_tenths":"20"`, `"ſpent_tenths":"0","spent_tenths":"20"`},
		{"missing reconciliation", `"reconciliation_required":false,`, ``},
		{"closed", `"closed":false`, `"closed":true`},
		{"unknown outcome", `"reconciliation_required":false`, `"reconciliation_required":true`},
		{"wrong task", `00000000-0000-4000-8000-000000000001`, `00000000-0000-4000-8000-000000000005`},
		{"wrong owner", `"owner_id":"7"`, `"owner_id":"8"`},
		{"wrong key", `"api_key_ids":["11"]`, `"api_key_ids":["12"]`},
		{"additional key", `"api_key_ids":["11"]`, `"api_key_ids":["11","12"]`},
		{"duplicate key", `"api_key_ids":["11"]`, `"api_key_ids":["11","11"]`},
		{"changed cap", `"limit_tenths":"100"`, `"limit_tenths":"101"`},
		{"unknown usage", `"spent_tenths":"20",`, ``},
		{"negative usage", `"spent_tenths":"20"`, `"spent_tenths":"-20"`},
		{"zero remaining", `"held_tenths":"0","remaining_tenths":"80"`, `"held_tenths":"80","remaining_tenths":"0"`},
		{"exhausted", `"spent_tenths":"20"`, `"spent_tenths":"100"`},
		{"overflow", `"held_tenths":"0"`, `"held_tenths":"9223372036854775808"`},
		{"inconsistent remaining", `"remaining_tenths":"80"`, `"remaining_tenths":"90"`},
		{"noncanonical counter", `"spent_tenths":"20"`, `"spent_tenths":"+20"`},
		{"wrong policy", `"output_weight":50`, `"output_weight":1`},
		{"wrong version", `"version":1`, `"version":2`},
	} {
		test.Run(replacement.name, func(test *testing.T) {
			operator, binding, calls := gatewayFixtureResponse(test, func(route, response string) string {
				if strings.HasPrefix(route, "GET /api/v1/admin/task-quotas/") {
					return strings.ReplaceAll(response, replacement.from, replacement.to)
				}
				return response
			})
			grant, err := operator.Provision(context.Background(), "00000000-0000-4000-8000-000000000004", "codex", binding)
			if !errors.Is(err, ErrUnavailable) || grant.Key != "" || calls.Load() != 3 {
				test.Fatalf("unsafe snapshot admitted or retried: %v, calls=%d", err, calls.Load())
			}
		})
	}
}

func TestTaskGatewayRejectsUnsafeKeysBeforeProvision(test *testing.T) {
	for _, replacement := range []struct{ name, from, to string }{
		{"wrong owner", `"user_id":7`, `"user_id":8`},
		{"missing key", `"id":11`, `"id":12`},
		{"inactive", `"status":"active"`, `"status":"disabled"`},
		{"admin token", `owned-task-secret`, `owned-operator-secret`},
		{"header injection", `owned-task-secret`, `owned-task-secret\r\nInjected: true`},
		{"expired", `"status":"active"`, `"status":"active","expires_at":"2000-01-01T00:00:00Z"`},
		{"unbounded pagination", `"pages":1`, `"pages":2`},
		{"missing pagination", `,"pages":1`, ``},
		{"duplicate key", `"items":[`, `"items":[{"id":11,"user_id":7,"key":"owned-task-secret","status":"active"},`},
	} {
		test.Run(replacement.name, func(test *testing.T) {
			operator, binding, calls := gatewayFixtureResponse(test, func(route, response string) string {
				if strings.Contains(route, "/api-keys") {
					return strings.ReplaceAll(response, replacement.from, replacement.to)
				}
				return response
			})
			grant, err := operator.Provision(context.Background(), "00000000-0000-4000-8000-000000000004", "codex", binding)
			if !errors.Is(err, ErrUnavailable) || grant.Key != "" || calls.Load() != 1 {
				test.Fatalf("unsafe key reached provision: %v, calls=%d", err, calls.Load())
			}
		})
	}
}

func TestTaskGatewayRejectsMalformedResponse(test *testing.T) {
	for _, response := range []string{
		`{}`, `{"code":0,"data":null}`, `{"code":1,"data":{}}`, `{"code":0,"data":{}} {}`,
		`{"code":1,"code":0,"data":{}}`, `{"code":1,"CODE":0,"data":{}}`,
		`{"code":0,"data":{"items":[],"items":[],"pages":1}}`,
		`{"code":0,"data":{"items":[],"\u0069tems":[],"pages":1}}`,
		`{"code":0,"data":` + strings.Repeat(`[`, 65) + `0` + strings.Repeat(`]`, 65) + `}`,
		strings.Repeat(" ", 1<<20) + `{}`, `{"code":0,"data":{`,
	} {
		operator, binding, calls := gatewayFixtureResponse(test, func(string, string) string { return response })
		grant, err := operator.Provision(context.Background(), "00000000-0000-4000-8000-000000000004", "claude", binding)
		if !errors.Is(err, ErrUnavailable) || grant.Key != "" || calls.Load() != 1 {
			test.Fatalf("malformed operator response accepted: %v", err)
		}
	}
}

func TestTaskGatewayPolicyAndSecretIsolation(test *testing.T) {
	operator, binding, _ := gatewayFixture(test)
	policy, err := operator.Authorize("00000000-0000-4000-8000-000000000004", "codex", binding)
	if err != nil {
		test.Fatal(err)
	}
	separate, err := New(operator.baseURL, operator.token, []Policy{policy})
	if err != nil {
		test.Fatal(err)
	}
	policy.KeyIDs["codex"] = 999
	returned, err := separate.Authorize(policy.RuntimeID, "codex", binding)
	if err != nil || returned.KeyIDs["codex"] != 11 {
		test.Fatal("mutable caller policy changed authorization")
	}
	returned.KeyIDs["codex"] = 998
	returned, err = separate.Authorize(policy.RuntimeID, "codex", binding)
	if err != nil || returned.KeyIDs["codex"] != 11 {
		test.Fatal("mutable authorization result changed policy")
	}
	grant, err := separate.Provision(context.Background(), policy.RuntimeID, "codex", binding)
	if err != nil {
		test.Fatal(err)
	}
	for _, secret := range []any{grant, &grant, separate, *separate} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			if strings.Contains(fmt.Sprintf(format, secret), "secret") {
				test.Fatal("formatting exposes secret")
			}
		}
		encoded, err := json.Marshal(secret)
		if err != nil || strings.Contains(string(encoded), "secret") {
			test.Fatal("JSON exposes secret")
		}
	}
	for _, baseURL := range []string{"http://gateway.invalid", "http://192.0.2.1", "https://operator:secret@gateway.invalid", "https://gateway.invalid?", "https://gateway.invalid/path", "https://gateway.invalid#fragment"} {
		if _, err := New(baseURL, "owned-secret", nil); !errors.Is(err, ErrUnavailable) {
			test.Fatal("unsafe endpoint accepted")
		}
	}
	if _, err := separate.Provision(context.Background(), policy.RuntimeID, "unsupported", binding); !errors.Is(err, ErrUnavailable) {
		test.Fatal("unsupported provider accepted")
	}
	if _, err := separate.Provision(context.Background(), "00000000-0000-4000-8000-000000000005", "codex", binding); !errors.Is(err, ErrUnavailable) {
		test.Fatal("different runtime accepted")
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := separate.Provision(ctx, policy.RuntimeID, "codex", binding); !errors.Is(err, ErrUnavailable) {
		test.Fatal("cancelled authorization accepted")
	}
}

func TestTaskGatewayRedirectCannotLeakOperator(test *testing.T) {
	operator, binding, _ := gatewayFixture(test)
	forwarded := new(atomic.Int32)
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		forwarded.Add(1)
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	operator.baseURL = redirect.URL
	if grant, err := operator.Provision(context.Background(), "00000000-0000-4000-8000-000000000004", "codex", binding); !errors.Is(err, ErrUnavailable) || grant.Key != "" || forwarded.Load() != 0 {
		test.Fatalf("redirect was followed: %v forwarded=%d", err, forwarded.Load())
	}
}

func TestTaskGatewayDistinctProviderKeysAreBothAuthorized(test *testing.T) {
	operator, binding, _ := gatewayFixture(test)
	policy, err := operator.Authorize("00000000-0000-4000-8000-000000000004", "claude", binding)
	if err != nil {
		test.Fatal(err)
	}
	policy.KeyIDs["codex"] = 12
	for _, active := range []bool{true, false} {
		test.Run(fmt.Sprint(active), func(test *testing.T) {
			calls := new(atomic.Int32)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				switch request.Method {
				case http.MethodPut:
					var body struct {
						Keys []int64 `json:"api_key_ids"`
					}
					if json.NewDecoder(request.Body).Decode(&body) != nil || len(body.Keys) != 2 || body.Keys[0] != 11 || body.Keys[1] != 12 {
						test.Error("both provider keys were not bound")
					}
					fmt.Fprint(writer, `{"code":0,"data":{"task_id":"`+binding.TaskID+`"}}`)
				case http.MethodGet:
					if strings.Contains(request.URL.Path, "/api-keys") {
						status := "disabled"
						if active {
							status = "active"
						}
						fmt.Fprint(writer, `{"code":0,"data":{"items":[{"id":11,"user_id":7,"key":"owned-claude-secret","status":"active"},{"id":12,"user_id":7,"key":"owned-codex-secret","status":"`+status+`"}],"pages":1}}`)
					} else {
						fmt.Fprint(writer, `{"code":0,"data":{"task_id":"`+binding.TaskID+`","closed":false,"reconciliation_required":false,"policy":{"version":1,"unit":"weighted_token_tenths","input_weight":10,"cache_creation_weight":10,"cache_read_weight":1,"output_weight":50},"exact":{"owner_id":"7","api_key_ids":["12","11"],"limit_tenths":"100","spent_tenths":"20","held_tenths":"1","remaining_tenths":"79"}}}`)
					}
				default:
					test.Error("unexpected operator mutation")
				}
			}))
			defer server.Close()
			operator, err := New(server.URL, "owned-operator-secret", []Policy{policy})
			if err != nil {
				test.Fatal(err)
			}
			for provider, expectedKey := range map[string]string{"claude": "owned-claude-secret", "codex": "owned-codex-secret"} {
				grant, err := operator.Provision(context.Background(), policy.RuntimeID, provider, binding)
				if active && (err != nil || grant.Key != expectedKey) {
					test.Fatalf("provider-specific grant rejected: %v", err)
				}
				if !active && (!errors.Is(err, ErrUnavailable) || grant.Key != "") {
					test.Fatal("unusable alternate provider key reached provisioning")
				}
			}
			expectedCalls := int32(2)
			if active {
				expectedCalls = 6
			}
			if calls.Load() != expectedCalls {
				test.Fatal("unexpected retry or mutation")
			}
		})
	}
}

func TestTaskGatewayProvisionAuthorizedAndResume(test *testing.T) {
	operator, binding, calls := gatewayFixture(test)
	for _, provider := range []string{"claude", "codex", "claude"} {
		grant, err := operator.Provision(context.Background(), "00000000-0000-4000-8000-000000000004", provider, binding)
		if err != nil || grant.Binding != binding || grant.Key != "owned-task-secret" || grant.BaseURL != operator.baseURL {
			test.Fatalf("authorized same-task handoff: %v", err)
		}
		if strings.Contains(fmt.Sprint(grant), "owned-task-secret") {
			test.Fatal("grant formatting exposes credential")
		}
	}
	if calls.Load() != 9 {
		test.Fatalf("each resume must reauthorize and replay provisioning: %d", calls.Load())
	}
}

func TestTaskGatewayRefusesForgedIdentityBeforeHTTP(test *testing.T) {
	operator, binding, calls := gatewayFixture(test)
	for _, change := range []func(*credentialexec.Binding){
		func(value *credentialexec.Binding) { value.OwnerID = "00000000-0000-4000-8000-000000000005" },
		func(value *credentialexec.Binding) { value.TaskID = "00000000-0000-4000-8000-000000000005" },
		func(value *credentialexec.Binding) { value.WorkspaceID = "00000000-0000-4000-8000-000000000005" },
	} {
		forged := binding
		change(&forged)
		if _, err := operator.Provision(context.Background(), "00000000-0000-4000-8000-000000000004", "codex", forged); err == nil {
			test.Fatal("forged identity admitted")
		}
	}
	if calls.Load() != 0 {
		test.Fatal("untrusted identity reached operator")
	}
}

func TestTaskGatewayRefusesUnavailableAndNeverEchoesSecrets(test *testing.T) {
	for _, status := range []int{401, 403, 409, 429, 503} {
		test.Run(fmt.Sprint(status), func(test *testing.T) {
			operator, binding, _ := gatewayFixture(test)
			calls := new(atomic.Int32)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.WriteHeader(status)
				fmt.Fprint(writer, "owned-operator-secret owned-task-secret")
			}))
			defer server.Close()
			operator.baseURL = server.URL
			_, err := operator.Provision(context.Background(), "00000000-0000-4000-8000-000000000004", "codex", binding)
			if err == nil || strings.Contains(err.Error(), "secret") || calls.Load() != 1 {
				test.Fatalf("must fail closed without retry or secret echo: %v calls=%d", err, calls.Load())
			}
		})
	}
}
