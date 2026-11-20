package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const queueOrderTestWorkspaceID = "11111111-2222-3333-4444-555555555555"

func queueOrderTestServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_TOKEN", "test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", queueOrderTestWorkspaceID)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
}

// cmdFrom builds a throwaway command with its own copy of the real command's
// flags (so no value leaks between cases) plus the root's persistent flags.
func cmdFrom(real *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{Use: real.Name()}
	real.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Value.Type() {
		case "int":
			cmd.Flags().Int(f.Name, 0, f.Usage)
		case "bool":
			cmd.Flags().Bool(f.Name, false, f.Usage)
		case "stringArray":
			cmd.Flags().StringArray(f.Name, nil, f.Usage)
		default:
			cmd.Flags().String(f.Name, f.DefValue, f.Usage)
		}
	})
	for _, name := range []string{"workspace-id", "profile", "server-url"} {
		if cmd.Flags().Lookup(name) == nil {
			cmd.Flags().String(name, "", "")
		}
	}
	return cmd
}

func TestWorkspaceQueueOrderShowPrintsEffectiveValues(t *testing.T) {
	for name, tc := range map[string]struct {
		settings string
		want     map[string]any
	}{
		"unset":  {`{}`, map[string]any{"queue_order": "priority", "queue_aging_minutes": float64(30)}},
		"set":    {`{"queue_order":"project_rank","queue_aging_minutes":90}`, map[string]any{"queue_order": "project_rank", "queue_aging_minutes": float64(90)}},
		"others": {`{"compact_prompt":"x"}`, map[string]any{"queue_order": "priority", "queue_aging_minutes": float64(30)}},
	} {
		t.Run(name, func(t *testing.T) {
			queueOrderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/workspaces/"+queueOrderTestWorkspaceID {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(`{"id":"` + queueOrderTestWorkspaceID + `","settings":` + tc.settings + `}`))
			})
			out, err := captureStdout(t, func() error {
				return runWorkspaceQueueOrderShow(cmdFrom(workspaceQueueOrderShowCmd), nil)
			})
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output %q is not JSON: %v", out, err)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v (output %s)", k, got[k], v, out)
				}
			}
		})
	}
}

func TestWorkspaceQueueOrderSetPatchesOnlyNamedKeys(t *testing.T) {
	var gotBody map[string]any
	queueOrderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/workspaces/"+queueOrderTestWorkspaceID {
			http.NotFound(w, r)
			return
		}
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":"` + queueOrderTestWorkspaceID + `","settings":{"queue_order":"project_rank","queue_aging_minutes":45}}`))
	})
	cmd := cmdFrom(workspaceQueueOrderSetCmd)
	_ = cmd.Flags().Set("order", "project_rank")
	out, err := captureStdout(t, func() error { return runWorkspaceQueueOrderSet(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBody) != 1 || gotBody["queue_order"] != "project_rank" {
		t.Errorf("body = %v, want only queue_order", gotBody)
	}
	if !strings.Contains(out, "project_rank") || !strings.Contains(out, "45") {
		t.Errorf("output %q does not read the stored values back", out)
	}

	cmd = cmdFrom(workspaceQueueOrderSetCmd)
	_ = cmd.Flags().Set("aging-minutes", "45")
	if _, err := captureStdout(t, func() error { return runWorkspaceQueueOrderSet(cmd, nil) }); err != nil {
		t.Fatal(err)
	}
	if len(gotBody) != 1 || gotBody["queue_aging_minutes"] != float64(45) {
		t.Errorf("body = %v, want only queue_aging_minutes 45", gotBody)
	}
}

func TestWorkspaceQueueOrderSetRefusesBadValuesWithoutARequest(t *testing.T) {
	requests := 0
	queueOrderTestServer(t, func(w http.ResponseWriter, r *http.Request) { requests++ })
	for name, tc := range map[string]struct {
		flag, value string
		want        []string
	}{
		"mode": {"order", "fifo", []string{"priority", "project_rank"}},
		"low":  {"aging-minutes", "4", []string{"5", "720"}},
		"high": {"aging-minutes", "721", []string{"5", "720"}},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := cmdFrom(workspaceQueueOrderSetCmd)
			_ = cmd.Flags().Set(tc.flag, tc.value)
			_, err := captureStdout(t, func() error { return runWorkspaceQueueOrderSet(cmd, nil) })
			if err == nil {
				t.Fatal("accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
	cmd := cmdFrom(workspaceQueueOrderSetCmd)
	if _, err := captureStdout(t, func() error { return runWorkspaceQueueOrderSet(cmd, nil) }); err == nil {
		t.Error("set with no flag accepted")
	}
	if requests != 0 {
		t.Errorf("%d requests sent for refused input", requests)
	}
}

func TestWorkspaceQueueOrderIsRegistered(t *testing.T) {
	found, _, err := workspaceCmd.Find([]string{"queue-order", "set"})
	if err != nil || found != workspaceQueueOrderSetCmd {
		t.Fatalf("workspace queue-order set not registered: %v", err)
	}
	found, _, err = workspaceCmd.Find([]string{"queue-order", "show"})
	if err != nil || found != workspaceQueueOrderShowCmd {
		t.Fatalf("workspace queue-order show not registered: %v", err)
	}
}

func TestProjectCreateSendsPriority(t *testing.T) {
	var gotBody map[string]any
	queueOrderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects" {
			http.NotFound(w, r)
			return
		}
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":"p1","title":"T","status":"planned","priority":"high"}`))
	})
	cmd := cmdFrom(projectCreateCmd)
	_ = cmd.Flags().Set("title", "T")
	_ = cmd.Flags().Set("priority", "high")
	if _, err := captureStdout(t, func() error { return runProjectCreate(cmd, nil) }); err != nil {
		t.Fatal(err)
	}
	if gotBody["priority"] != "high" {
		t.Errorf("body = %v, want priority high", gotBody)
	}

	cmd = cmdFrom(projectCreateCmd)
	_ = cmd.Flags().Set("title", "T")
	_ = cmd.Flags().Set("priority", "huge")
	_, err := captureStdout(t, func() error { return runProjectCreate(cmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "urgent, high, medium, low, none") {
		t.Errorf("bad priority error = %v, want the allowed values", err)
	}
}

func TestProjectUpdateSendsPriority(t *testing.T) {
	const projectID = "99999999-8888-7777-6666-555555555555"
	var gotBody map[string]any
	queueOrderTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/projects/"+projectID,
			r.Method == http.MethodPatch && r.URL.Path == "/api/projects/"+projectID:
			gotBody = nil
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = w.Write([]byte(`{"id":"` + projectID + `","title":"T","status":"planned","priority":"urgent"}`))
		default:
			http.NotFound(w, r)
		}
	})
	cmd := cmdFrom(projectUpdateCmd)
	_ = cmd.Flags().Set("priority", "urgent")
	if _, err := captureStdout(t, func() error { return runProjectUpdate(cmd, []string{projectID}) }); err != nil {
		t.Fatal(err)
	}
	if len(gotBody) != 1 || gotBody["priority"] != "urgent" {
		t.Errorf("body = %v, want only priority urgent", gotBody)
	}

	cmd = cmdFrom(projectUpdateCmd)
	_ = cmd.Flags().Set("priority", "huge")
	_, err := captureStdout(t, func() error { return runProjectUpdate(cmd, []string{projectID}) })
	if err == nil || !strings.Contains(err.Error(), "urgent, high, medium, low, none") {
		t.Errorf("bad priority error = %v, want the allowed values", err)
	}
}
