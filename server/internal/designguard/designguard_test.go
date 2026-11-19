package designguard

import (
	"errors"
	"testing"
)

const origin = "https://doop.example.com"

func code(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	if err != nil {
		return "other:" + err.Error()
	}
	return ""
}

func TestParseSettings(t *testing.T) {
	const uuidA = "b73a1103-30d7-4f9b-9412-4edcd64f5232"
	const uuidB = "8394690d-632a-4b15-a333-c9f0247b0985"
	good := `{"design_card_guard":{"mode":"block","doop_origin":"https://Doop.Example.com/","design_url_property_id":"` + uuidA + `","design_workflow_id":"` + uuidB + `"}}`
	got, err := ParseSettings([]byte(good))
	if err != nil {
		t.Fatalf("good: %v", err)
	}
	want := Settings{Mode: "block", DoopOrigin: origin, DesignURLPropertyID: uuidA, DesignWorkflowID: uuidB}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	for _, raw := range []string{"", `{}`, `{"other":1}`, `{"design_card_guard":{}}`, `{"design_card_guard":{"mode":"off"}}`} {
		got, err := ParseSettings([]byte(raw))
		if err != nil || got.Mode != "off" {
			t.Errorf("%q: got %+v, %v; want mode off", raw, got, err)
		}
	}

	warnNoWorkflow := `{"design_card_guard":{"mode":"warn","doop_origin":"https://doop.example.com:8443","design_url_property_id":"` + uuidA + `"}}`
	if got, err := ParseSettings([]byte(warnNoWorkflow)); err != nil || got.DoopOrigin != "https://doop.example.com:8443" || got.DesignWorkflowID != "" {
		t.Errorf("warn without workflow: %+v, %v", got, err)
	}

	bad := map[string]string{
		"null value":          `{"design_card_guard":null}`,
		"array value":         `{"design_card_guard":[]}`,
		"mode on":             `{"design_card_guard":{"mode":"on"}}`,
		"mode null":           `{"design_card_guard":{"mode":null}}`,
		"mode number":         `{"design_card_guard":{"mode":1}}`,
		"unknown field":       `{"design_card_guard":{"mode":"off","extra":1}}`,
		"block no origin":     `{"design_card_guard":{"mode":"block","design_url_property_id":"` + uuidA + `"}}`,
		"block no property":   `{"design_card_guard":{"mode":"block","doop_origin":"` + origin + `"}}`,
		"http origin":         `{"design_card_guard":{"mode":"off","doop_origin":"http://doop.example.com"}}`,
		"origin with path":    `{"design_card_guard":{"mode":"off","doop_origin":"https://doop.example.com/c/x"}}`,
		"origin with query":   `{"design_card_guard":{"mode":"off","doop_origin":"https://doop.example.com?x=1"}}`,
		"origin userinfo":     `{"design_card_guard":{"mode":"off","doop_origin":"https://u:p@doop.example.com"}}`,
		"origin not url":      `{"design_card_guard":{"mode":"off","doop_origin":"doop"}}`,
		"property not a uuid": `{"design_card_guard":{"mode":"off","design_url_property_id":"abc"}}`,
		"workflow not a uuid": `{"design_card_guard":{"mode":"off","design_workflow_id":"abc"}}`,
		"settings not json":   `[`,
	}
	for name, raw := range bad {
		if got, err := ParseSettings([]byte(raw)); err == nil {
			t.Errorf("%s: accepted, got %+v", name, got)
		}
	}
}

func TestParseFrameURL(t *testing.T) {
	ok := map[string]FrameRef{
		origin + "/c/file1?frame=f1":                        {Canvas: "file1", Frame: "f1"},
		origin + "/c/file1/proto?frame=f1":                  {Canvas: "file1", Frame: "f1"},
		origin + "/c/file1?frame=f1&page=p1":                {Canvas: "file1", Page: "p1", Frame: "f1"},
		origin + "/c/file1?page=p1&frame=f_1-x&utm=a&utm=b": {Canvas: "file1", Page: "p1", Frame: "f_1-x"},
		origin + "/c/file1?frame=f1#top":                    {Canvas: "file1", Frame: "f1"},
		"HTTPS://DOOP.example.com/c/file1?frame=f1":         {Canvas: "file1", Frame: "f1"},
	}
	for raw, want := range ok {
		got, err := ParseFrameURL(origin, raw)
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v; want %+v", raw, got, err, want)
		}
	}
	bad := map[string]string{
		"":                                              CodeDesignFrameURLInvalid,
		"not a url":                                     CodeDesignFrameURLInvalid,
		origin + "/c/file1":                             CodeDesignFrameURLInvalid,
		origin + "/c/file1?page=p1":                     CodeDesignFrameURLInvalid,
		origin + "/c/file1?frame=":                      CodeDesignFrameURLInvalid,
		origin + "/c/file1?frame=a%20b":                 CodeDesignFrameURLInvalid,
		origin + "/c/?frame=f1":                         CodeDesignFrameURLInvalid,
		origin + "/c/file1/other?frame=f1":              CodeDesignFrameURLInvalid,
		origin + "/c/file1/proto/x?frame=f1":            CodeDesignFrameURLInvalid,
		origin + "/d/file1?frame=f1":                    CodeDesignFrameURLInvalid,
		origin + "/c/file1%2Fx?frame=f1":                CodeDesignFrameURLInvalid,
		"https://evil.example.com/c/file1?frame=f1":     CodeDesignFrameURLInvalid,
		"https://doop.example.com.evil.io/c/f?frame=f1": CodeDesignFrameURLInvalid,
		"http://doop.example.com/c/file1?frame=f1":      CodeDesignFrameURLInvalid,
		"https://u@doop.example.com/c/file1?frame=f1":   CodeDesignFrameURLInvalid,
		"https://doop.example.com:444/c/file1?frame=f1": CodeDesignFrameURLInvalid,
		"javascript:alert(1)":                           CodeDesignFrameURLInvalid,
		origin + "/c/file1?frame=f1&frame=f2":           CodeDesignURLAmbiguous,
		origin + "/c/file1?frame=f1&frame=f1":           CodeDesignURLAmbiguous,
		origin + "/c/file1?frame=f1&page=p1&page=p2":    CodeDesignURLAmbiguous,
	}
	for raw, want := range bad {
		if _, err := ParseFrameURL(origin, raw); code(err) != want {
			t.Errorf("%q: code %q, want %q", raw, code(err), want)
		}
	}
}

func TestParseDesignURL(t *testing.T) {
	ok := map[string]FrameRef{
		origin + "/c/file1?frame=f1":               {Canvas: "file1", Frame: "f1"},
		origin + "/c/file1?page=p1":                {Canvas: "file1", Page: "p1"},
		origin + "/c/file1/proto?page=p1&frame=f1": {Canvas: "file1", Page: "p1", Frame: "f1"},
		"  " + origin + "/c/file1?frame=f1\n":      {Canvas: "file1", Frame: "f1"},
	}
	for raw, want := range ok {
		got, err := ParseDesignURL(origin, raw)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v; want %+v", raw, got, err, want)
		}
	}
	bad := map[string]string{
		"":                                    CodeDesignURLMissing,
		"   ":                                 CodeDesignURLMissing,
		"https://figma.com/file/x":            CodeDesignURLMissing,
		origin + "/c/file1":                   CodeDesignURLMissing,
		origin + "/c/file1?frame=f1&frame=f2": CodeDesignURLAmbiguous,
		origin + "/c/file1?page=p1&page=p2":   CodeDesignURLAmbiguous,
		origin + "/c/file1?frame=%zz":         CodeDesignURLMissing,
	}
	for raw, want := range bad {
		if _, err := ParseDesignURL(origin, raw); code(err) != want {
			t.Errorf("%q: code %q, want %q", raw, code(err), want)
		}
	}
}

func TestHasDoopLink(t *testing.T) {
	yes := []string{
		origin + "/c/file1?frame=f1",
		"See " + origin + "/c/file1/proto?frame=f1 for the frame.",
		"[frame](" + origin + "/c/file1?frame=f1)",
		"<" + origin + "/c/file1>",
		"line one\n" + origin + "/c/file1.",
		// T3 matches any link on the origin, whatever its path or query: a malformed
		// frame link must still trigger the guard, never skip it.
		origin,
		origin + "/",
		origin + "/c/file1?frame=",
		origin + "/c/file1?frame=a%20b&frame=c",
		origin + "/c/?frame=f1",
		origin + "/api/x",
		"see (" + origin + "/c/file1/other).",
		"HTTPS://DOOP.example.com/c/file1",
		"http://doop.example.com/c/file1",
		"https://u@doop.example.com/c/file1",
		"https://doop.example.com:443/c/file1",
	}
	for _, text := range yes {
		if !HasDoopLink(origin, text) {
			t.Errorf("want link in %q", text)
		}
	}
	no := []string{
		"",
		"doop",
		"the doop.example.com design",
		"https://evil.example.com/c/file1?frame=f1",
		"https://doop.example.com.evil.io/c/file1",
		"https://doop.example.com:8443/c/file1",
		"https://notdoop.example.com/c/file1",
		"mention of /c/file1 only",
	}
	for _, text := range no {
		if HasDoopLink(origin, text) {
			t.Errorf("no link expected in %q", text)
		}
	}
	if HasDoopLink("", origin+"/c/file1") {
		t.Error("empty origin must never match")
	}
}
