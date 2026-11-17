package intakehold

import (
	"strings"
	"testing"
)

const validEntry = `{"project_id":"5e621e12-eeb4-40a5-9571-25657d5b0ec5","mode":"block","starts_at":"2026-11-17T16:14:52+07:00","ends_at":"2026-11-23T00:00:00+07:00","exempt_types":["fix","bug"],"exempt_labels":["bug"],"exempt_escalation_classes":["spend","deploy","publish","irreversible","product_direction"],"exempt_autopilot_ids":["1dc08d8e-58e4-4ab3-9ebc-3596324a5e4d"],"exempt_issue_ids":["01a1e90c-cb5a-7e37-abba-ead8e1db017f"],"source":"01a1e8c9-a7af-7a16-9f67-bf2678c8b047"}`

func TestValidate(t *testing.T) {
	if err := Validate([]byte("[" + validEntry + "]")); err != nil {
		t.Fatalf("valid entry: %v", err)
	}
	if err := Validate([]byte(`[{"project_id":"5e621e12-eeb4-40a5-9571-25657d5b0ec5","mode":"off","starts_at":"2026-11-17T16:14:52+07:00","ends_at":"2026-11-23T00:00:00+07:00","source":"s"}]`)); err != nil {
		t.Fatalf("minimal entry: %v", err)
	}
	if err := Validate([]byte(`[]`)); err != nil {
		t.Fatalf("empty list: %v", err)
	}
	bad := map[string]string{
		"not an array":            `{}`,
		"unknown key":             strings.Replace(validEntry, `"mode"`, `"bogus":1,"mode"`, 1),
		"bad starts_at":           strings.Replace(validEntry, "2026-11-17T16:14:52+07:00", "yesterday", 1),
		"bad ends_at":             strings.Replace(validEntry, "2026-11-23T00:00:00+07:00", "2026-11-23", 1),
		"missing starts_at":       strings.Replace(validEntry, `"starts_at":"2026-11-17T16:14:52+07:00",`, "", 1),
		"ends_at not after":       strings.Replace(validEntry, "2026-11-23T00:00:00+07:00", "2026-11-17T16:14:52+07:00", 1),
		"bad project uuid":        strings.Replace(validEntry, "5e621e12-eeb4-40a5-9571-25657d5b0ec5", "nope", 1),
		"bad autopilot uuid":      strings.Replace(validEntry, "1dc08d8e-58e4-4ab3-9ebc-3596324a5e4d", "nope", 1),
		"bad issue uuid":          strings.Replace(validEntry, "01a1e90c-cb5a-7e37-abba-ead8e1db017f", "nope", 1),
		"unknown mode":            strings.Replace(validEntry, `"block"`, `"freeze"`, 1),
		"unknown class":           strings.Replace(validEntry, `"spend"`, `"reversible"`, 1),
		"uppercase type":          strings.Replace(validEntry, `"fix"`, `"Fix"`, 1),
		"empty label":             strings.Replace(validEntry, `"exempt_labels":["bug"]`, `"exempt_labels":[""]`, 1),
		"types not strings":       strings.Replace(validEntry, `"exempt_types":["fix","bug"]`, `"exempt_types":[1]`, 1),
		"missing source":          strings.Replace(validEntry, `,"source":"01a1e8c9-a7af-7a16-9f67-bf2678c8b047"`, "", 1),
		"null source":             strings.Replace(validEntry, `"source":"01a1e8c9-a7af-7a16-9f67-bf2678c8b047"`, `"source":null`, 1),
		"uppercase project uuid":  strings.Replace(validEntry, "5e621e12-eeb4-40a5-9571-25657d5b0ec5", "5E621E12-EEB4-40A5-9571-25657D5B0EC5", 1),
		"braced issue uuid":       strings.Replace(validEntry, "01a1e90c-cb5a-7e37-abba-ead8e1db017f", "{01a1e90c-cb5a-7e37-abba-ead8e1db017f}", 1),
		"unhyphenated autopilot":  strings.Replace(validEntry, "1dc08d8e-58e4-4ab3-9ebc-3596324a5e4d", "1dc08d8e58e44ab39ebc3596324a5e4d", 1),
		"empty source":            strings.Replace(validEntry, `"source":"01a1e8c9-a7af-7a16-9f67-bf2678c8b047"`, `"source":""`, 1),
		"two entries one project": "[" + validEntry + "," + validEntry + "]",
	}
	for name, raw := range bad {
		if !strings.HasPrefix(raw, "[") && name != "not an array" {
			raw = "[" + raw + "]"
		}
		t.Run(name, func(t *testing.T) {
			if err := Validate([]byte(raw)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestParseStoredIgnoresMalformed(t *testing.T) {
	if e := ParseStored([]byte(`[`+validEntry+`]`), "5e621e12-eeb4-40a5-9571-25657d5b0ec5"); e == nil || e.Mode != ModeBlock {
		t.Fatalf("entry %+v", e)
	}
	if e := ParseStored([]byte(`[`+validEntry+`]`), "other"); e != nil {
		t.Fatalf("project with no entry: %+v", e)
	}
	for _, raw := range []string{`"junk"`, `[{"mode":"block"}]`, `{`, ``} {
		if e := ParseStored([]byte(raw), "5e621e12-eeb4-40a5-9571-25657d5b0ec5"); e != nil {
			t.Fatalf("malformed %q holds: %+v", raw, e)
		}
	}
}
