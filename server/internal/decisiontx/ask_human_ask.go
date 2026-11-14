package decisiontx

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Guard for a card that asks a human to act (CHE-5753, retro row R03 of
// CHE-5575). A step is refused when it
//   - cites a UI control path and says nowhere it was observed (observed),
//   - names a repository file without the repo and the 40-character commit SHA
//     (files), unless the file comes from an attachment,
//   - carries a command that was not dry-run on the host shape it targets
//     (dry_run), or
//   - carries a placeholder the human must edit.

// StepFile pins a file a step names: a commit in a repository, or an attachment.
type StepFile struct {
	Path         string `json:"path"`
	Repo         string `json:"repo,omitempty"`
	SHA          string `json:"sha,omitempty"`
	AttachmentID string `json:"attachment_id,omitempty"`
}

// StepDryRun is the proof a command ran on the host shape the human will use.
type StepDryRun struct {
	Host   string `json:"host"`
	Result string `json:"result"`
}

var (
	stepFileFields   = []string{"path", "repo", "sha", "attachment_id"}
	stepDryRunFields = []string{"host", "result"}

	// An arrow between two words, or a spaced ">" ("Settings > API Tokens").
	uiPath = regexp.MustCompile(`[\p{L}\p{N}]\s*(?:→|›|»|->)\s*[\p{L}\p{N}]|[\p{L}\p{N}] > [\p{L}\p{N}]`)

	urlText   = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	repoPath  = regexp.MustCompile(`(?:^|[\s'"(=])((?:\.{1,2}/)?(?:[\w.-]+/)+[\w-]+\.[A-Za-z][A-Za-z0-9]{0,7})\b`)
	hostFirst = regexp.MustCompile(`(?i)\.(?:com|io|net|org|dev|app)$`)

	commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repoSlug  = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
	notRun    = regexp.MustCompile(`(?i)^\s*(?:not\s+(?:run|tested|verified|done)|untested|none|n/?a|todo|tbd)\b`)

	// Placeholder words and an ellipsis; "main...HEAD" is a git range, not one.
	placeholderWord = regexp.MustCompile(`\b(?:YOUR_[A-Z0-9_]+|REPLACE_?ME|CHANGE_?ME|TODO|TBD|FIXME|X{3,})\b`)
	ellipsisText    = regexp.MustCompile(`…|(?:^|\s)\.\.\.`)
)

func decodeStepFiles(field string, raw json.RawMessage, into *[]StepFile) error {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return &AskError{field, "must be a list"}
	}
	for index, item := range items {
		prefix := fmt.Sprintf("%s[%d]", field, index)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			return &AskError{prefix, "must be an object with path, repo, sha, attachment_id"}
		}
		if err := rejectUnknown(prefix, fields, stepFileFields); err != nil {
			return err
		}
		var file StepFile
		for _, f := range []struct {
			name string
			into *string
		}{{"path", &file.Path}, {"repo", &file.Repo}, {"sha", &file.SHA}, {"attachment_id", &file.AttachmentID}} {
			if err := decodeString(prefix+"."+f.name, fields[f.name], f.into); err != nil {
				return err
			}
		}
		*into = append(*into, file)
	}
	return nil
}

func decodeDryRun(field string, raw json.RawMessage, into **StepDryRun) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return &AskError{field, "must be an object with host, result"}
	}
	if err := rejectUnknown(field, fields, stepDryRunFields); err != nil {
		return err
	}
	var run StepDryRun
	for _, f := range []struct {
		name string
		into *string
	}{{"host", &run.Host}, {"result", &run.Result}} {
		if err := decodeString(field+"."+f.name, fields[f.name], f.into); err != nil {
			return err
		}
	}
	*into = &run
	return nil
}

// validateAskEvidence runs the human-ask guard checks for one step.
func (step HumanStep) validateAskEvidence(prefix string) error {
	for _, check := range []func(string) error{step.checkPlaceholders, step.checkFiles, step.checkObserved, step.checkDryRun} {
		if err := check(prefix); err != nil {
			return err
		}
	}
	return nil
}

func (step HumanStep) checkPlaceholders(prefix string) error {
	for _, found := range []string{
		firstMatch(placeholder, step.Command), firstMatch(placeholderWord, step.Command),
		strings.TrimSpace(firstMatch(ellipsisText, step.Command)),
	} {
		if found != "" {
			return &AskError{prefix + ".command", "has a placeholder (" + found + "); write the literal the human runs"}
		}
	}
	return nil
}

func firstMatch(re *regexp.Regexp, text string) string { return re.FindString(text) }

func (step HumanStep) checkObserved(prefix string) error {
	text := step.Action
	for _, value := range step.Values {
		text += "\n" + value.Source
	}
	if uiPath.MatchString(text) && blank(step.Observed) {
		return &AskError{prefix, "cites a UI path with no observed; say where and when you saw the control"}
	}
	return nil
}

// repoFiles lists the relative file paths a step names, outside URLs. Absolute
// and home paths belong to the human's own host and stay free.
func (step HumanStep) repoFiles() []string {
	text := urlText.ReplaceAllString(step.Action+"\n"+step.Command, " ")
	var out []string
	for _, m := range repoPath.FindAllStringSubmatch(text, -1) {
		first, _, _ := strings.Cut(strings.TrimPrefix(m[1], "./"), "/")
		if hostFirst.MatchString(first) {
			continue
		}
		out = append(out, m[1])
	}
	return out
}

func (step HumanStep) checkFiles(prefix string) error {
	for index, file := range step.Files {
		at := fmt.Sprintf("%s.files[%d]", prefix, index)
		switch {
		case blank(file.Path):
			return &AskError{at + ".path", "is required"}
		case !blank(file.AttachmentID):
		case blank(file.Repo) || blank(file.SHA):
			return &AskError{at, "needs a repo and sha, or an attachment_id"}
		case !repoSlug.MatchString(file.Repo):
			return &AskError{at + ".repo", "must be owner/name"}
		case !commitSHA.MatchString(file.SHA):
			return &AskError{at + ".sha", "must be a 40-character commit SHA"}
		}
	}
	for _, name := range step.repoFiles() {
		if !step.pins(name) {
			return &AskError{prefix, "names the file " + name + " without a files entry with repo and a 40-character commit SHA"}
		}
	}
	return nil
}

func (step HumanStep) pins(name string) bool {
	name = strings.TrimPrefix(name, "./")
	for _, file := range step.Files {
		if strings.TrimPrefix(file.Path, "./") == name {
			return true
		}
	}
	return false
}

func (step HumanStep) checkDryRun(prefix string) error {
	if blank(step.Command) {
		return nil
	}
	at := prefix + ".dry_run"
	switch run := step.DryRun; {
	case run == nil:
		return &AskError{at, "is required for a command; say the host shape you ran it on and what it printed"}
	case blank(run.Host):
		return &AskError{at + ".host", "is required"}
	case blank(run.Result):
		return &AskError{at + ".result", "is required"}
	case notRun.MatchString(run.Result):
		return &AskError{at + ".result", "says the command was not run; run it on the target host shape first"}
	}
	return nil
}

// askEvidenceLines renders what a step proved, placed before "Done when".
func (step HumanStep) askEvidenceLines() string {
	var out strings.Builder
	if !blank(step.Observed) {
		fmt.Fprintf(&out, "   - Seen at: %s\n", step.Observed)
	}
	for _, file := range step.Files {
		if blank(file.AttachmentID) {
			fmt.Fprintf(&out, "   - File: %s in %s at %s\n", file.Path, file.Repo, file.SHA)
		} else {
			fmt.Fprintf(&out, "   - File: %s from attachment %s\n", file.Path, file.AttachmentID)
		}
	}
	if step.DryRun != nil {
		fmt.Fprintf(&out, "   - Dry run on %s: %s\n", step.DryRun.Host, step.DryRun.Result)
	}
	return out.String()
}
