package ghrun

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	oldSHA = "1111111111111111111111111111111111111111"
	newSHA = "2222222222222222222222222222222222222222"
	digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// releaseJob is a completed, successful cd-deploy `deploy` job with a receipt.
func releaseJob(mutate func(job map[string]any)) []byte {
	job := map[string]any{
		"id": 7, "run_id": 9, "run_attempt": 1, "name": "release-receipt", "head_sha": newSHA,
		"workflow_name": "CD deploy to C00", "head_branch": "main", "conclusion": "success",
		"steps": []any{
			map[string]any{"name": "Deploy to C00 on local runner", "conclusion": "success"},
			map[string]any{"name": "release-receipt old=" + oldSHA + " new=" + newSHA + " digest=" + digest, "conclusion": "success"},
		},
	}
	if mutate != nil {
		mutate(job)
	}
	body, _ := json.Marshal(map[string]any{
		"action": "completed", "workflow_job": job,
		"repository":   map[string]any{"name": "Multica", "owner": map[string]any{"login": "Cheese-Work"}},
		"installation": map[string]any{"id": 5},
	})
	return body
}

func setStep(index int, field string, value any) func(map[string]any) {
	return func(job map[string]any) { job["steps"].([]any)[index].(map[string]any)[field] = value }
}

func TestParseReleaseReceiptAcceptsOnlyAVerifiedDeploy(t *testing.T) {
	got, ok := ParseReleaseReceipt(releaseJob(nil))
	want := ReleaseReceipt{InstallationID: 5, RepoOwner: "cheese-work", RepoName: "multica", RunID: 9, OldSHA: oldSHA, NewSHA: newSHA, Digest: digest}
	if !ok || got != want {
		t.Fatalf("verified deploy = %+v ok=%v, want %+v", got, ok, want)
	}
	for name, mutate := range map[string]func(map[string]any){
		"job failed":                     func(j map[string]any) { j["conclusion"] = "failure" },
		"job skipped":                    func(j map[string]any) { j["conclusion"] = "skipped"; delete(j, "steps") },
		"receipt step skipped":           setStep(1, "conclusion", "skipped"),
		"receipt step failed":            setStep(1, "conclusion", "failure"),
		"other workflow":                 func(j map[string]any) { j["workflow_name"] = "CI" },
		"other job":                      func(j map[string]any) { j["name"] = "build" },
		"deploy job (readback not done)": func(j map[string]any) { j["name"] = "deploy" },
		"not main":                       func(j map[string]any) { j["head_branch"] = "feature" },
		"no steps":                       func(j map[string]any) { delete(j, "steps") },
		"short new sha":                  setStep(1, "name", "release-receipt old="+oldSHA+" new=abc digest="+digest),
		"bad digest":                     setStep(1, "name", "release-receipt old="+oldSHA+" new="+newSHA+" digest=sha256:zz"),
		"extra text":                     setStep(1, "name", "release-receipt old="+oldSHA+" new="+newSHA+" digest="+digest+"; rm -rf"),
		"unevaluated name":               setStep(1, "name", "release-receipt old=${{steps.receipt.outputs.old}} new=x digest=y"),
	} {
		if _, ok := ParseReleaseReceipt(releaseJob(mutate)); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, body := range map[string][]byte{"not json": []byte("{"), "empty": []byte("{}")} {
		if _, ok := ParseReleaseReceipt(body); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	// A job still running is not a receipt, whatever its steps say.
	if _, ok := ParseReleaseReceipt([]byte(strings.Replace(string(releaseJob(nil)), `"action":"completed"`, `"action":"in_progress"`, 1))); ok {
		t.Error("in_progress delivery accepted")
	}
}
