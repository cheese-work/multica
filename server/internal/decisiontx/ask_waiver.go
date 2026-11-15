package decisiontx

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// TopicTestFirstWaiver is the only topic: a test-first waiver ask (CHE-4383 B2a).
// The topic list is closed, so System 1 can tell a waiver ask from any other ask of the same class.
const TopicTestFirstWaiver = "test_first_waiver"

// Option keys of a waiver ask. A position says nothing about them: the server moves the recommended option first.
const (
	OptionKeyWaive  = "waive"
	OptionKeyRefuse = "refuse"
)

// Waiver is the typed block of a waiver ask. Every field is a closed value the asking agent chooses; none is free text.
type Waiver struct {
	Repo               string `json:"repo"`
	HeadSHA            string `json:"head_sha"`
	RedImpossibleCat   string `json:"red_impossible_category"`
	WeakerEvidenceKind string `json:"weaker_evidence_kind"`
	CrossLabPassOnHead bool   `json:"cross_lab_pass_on_head"`
	TestsRetrospective bool   `json:"tests_retrospective"`
	ChangeKind         string `json:"change_kind"`
}

var (
	waiverFields     = []string{"repo", "head_sha", "red_impossible_category", "weaker_evidence_kind", "cross_lab_pass_on_head", "tests_retrospective", "change_kind"}
	waiverRepo       = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	waiverHeadSHA    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	waiverCategories = []string{"no_behavior_change", "missing_test_harness", "historical_commit", "external_dependency", "other"}
	waiverEvidence   = []string{"characterization_test", "manual_repro", "static_check", "review_only"}
	waiverChanges    = []string{"docs", "tests_only", "dependency", "new_file", "code"}
)

func decodeWaiver(raw json.RawMessage, into **Waiver) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	fields, err := decodeObject(raw)
	if err != nil {
		return &AskError{"ask.waiver", "must be an object"}
	}
	if err := rejectUnknown("ask.waiver", fields, waiverFields); err != nil {
		return err
	}
	var waiver Waiver
	for _, field := range []struct {
		name string
		into *string
	}{{"repo", &waiver.Repo}, {"head_sha", &waiver.HeadSHA}, {"red_impossible_category", &waiver.RedImpossibleCat}, {"weaker_evidence_kind", &waiver.WeakerEvidenceKind}, {"change_kind", &waiver.ChangeKind}} {
		if err := decodeString("ask.waiver."+field.name, fields[field.name], field.into); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name string
		into *bool
	}{{"cross_lab_pass_on_head", &waiver.CrossLabPassOnHead}, {"tests_retrospective", &waiver.TestsRetrospective}} {
		// A missing boolean is refused, not read as false: it is evidence the caller must state.
		if raw := fields[field.name]; string(raw) == "null" || json.Unmarshal(raw, field.into) != nil {
			return &AskError{"ask.waiver." + field.name, "is required and must be true or false"}
		}
	}
	*into = &waiver
	return nil
}

// validateWaiverContract holds the topic rules. The waiver block, option keys and topic exist together or not at all.
func (ask Ask) validateWaiverContract() error {
	if ask.Topic == "" {
		if ask.Waiver != nil {
			return &AskError{"ask.waiver", "requires ask.topic " + TopicTestFirstWaiver}
		}
		for index, option := range ask.Options {
			if option.OptionKey != "" {
				return &AskError{fmt.Sprintf("ask.options[%d].option_key", index), "requires ask.topic " + TopicTestFirstWaiver}
			}
		}
		return nil
	}
	if ask.Topic != TopicTestFirstWaiver {
		return &AskError{"ask.topic", "must be " + TopicTestFirstWaiver}
	}
	if ask.Waiver == nil {
		return &AskError{"ask.waiver", "is required with ask.topic " + TopicTestFirstWaiver}
	}
	waiver := ask.Waiver
	for _, check := range []struct {
		name    string
		problem string
		ok      bool
	}{
		{"repo", "must be owner/name", waiverRepo.MatchString(waiver.Repo)},
		{"head_sha", "must be 40 lowercase hex characters", waiverHeadSHA.MatchString(waiver.HeadSHA)},
		{"red_impossible_category", "must be one of " + joinKeys(waiverCategories), slices.Contains(waiverCategories, waiver.RedImpossibleCat)},
		{"weaker_evidence_kind", "must be one of " + joinKeys(waiverEvidence), slices.Contains(waiverEvidence, waiver.WeakerEvidenceKind)},
		{"change_kind", "must be one of " + joinKeys(waiverChanges), slices.Contains(waiverChanges, waiver.ChangeKind)},
	} {
		if !check.ok {
			return &AskError{"ask.waiver." + check.name, check.problem}
		}
	}
	if len(ask.Options) != 2 {
		return &AskError{"ask.options", "requires exactly two options (waive and refuse) with ask.topic " + TopicTestFirstWaiver}
	}
	keys := []string{ask.Options[0].OptionKey, ask.Options[1].OptionKey}
	slices.Sort(keys)
	if keys[0] != OptionKeyRefuse || keys[1] != OptionKeyWaive {
		return &AskError{"ask.options", "must carry option_key waive on one option and refuse on the other"}
	}
	return nil
}

func joinKeys(keys []string) string { return strings.Join(keys, ", ") }
