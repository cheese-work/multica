package issueworkflow

import (
	"testing"
	"time"
)

const quickFirstOK = `{"workflow_id":"7328545d-f16f-4bdb-9deb-27f5d70b1210","types":["feat","fix"],"starts_at":"2026-11-20T00:00:00Z","ends_at":"2026-12-11T00:00:00Z"}`

func TestDecodeQuickFirst(t *testing.T) {
	qf := DecodeQuickFirst([]byte(`{"quick_first":` + quickFirstOK + `}`))
	if qf == nil || !qf.Lists("fix") || qf.Lists("docs") {
		t.Fatalf("decoded %+v", qf)
	}
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if !qf.Open(start) || qf.Open(start.Add(-time.Nanosecond)) || qf.Open(qf.EndsAt) || !qf.Open(qf.EndsAt.Add(-time.Nanosecond)) {
		t.Fatal("the window is not [starts_at, ends_at)")
	}
	for name, settings := range map[string]string{
		"missing": `{}`, "null": `{"quick_first":null}`, "invalid": `{"quick_first":{"types":[]}}`, "not json": `x`,
	} {
		if DecodeQuickFirst([]byte(settings)) != nil {
			t.Errorf("%s: decoded a value, want the arm off", name)
		}
	}
}

func TestValidateQuickFirst(t *testing.T) {
	if err := ValidateQuickFirst([]byte(quickFirstOK)); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"empty window":     `{"workflow_id":"7328545d-f16f-4bdb-9deb-27f5d70b1210","types":["feat"],"starts_at":"2026-11-20T00:00:00Z","ends_at":"2026-11-20T00:00:00Z"}`,
		"trailing garbage": quickFirstOK + `{}`,
	} {
		if ValidateQuickFirst([]byte(raw)) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
