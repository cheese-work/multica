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

	urlText  = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	repoPath = regexp.MustCompile("(?:^|[\\s'\"(=`])((?:\\.{1,2}/)?(?:[\\w.-]+/)+[\\w-]+\\.[A-Za-z][A-Za-z0-9]{0,7})\\b")
	// A file in the repo root: setup.sh, ./Makefile.mk. Only known file extensions, so a domain is not a file.
	rootFile = regexp.MustCompile("(?:^|[\\s'\"(=`])((?:\\.{1,2}/)?[\\w-][\\w.-]*\\.(?:sh|bash|zsh|md|json|ya?ml|toml|go|py|ts|tsx|jsx?|mjs|sql|kts?|gradle|xml|txt|env|properties|conf|cfg|ini|mod|sum|lock))\\b")
	// "from feature/foo.bar", "into release/1.2": a branch, not a file.
	gitRefBefore = regexp.MustCompile("(?i)\\b(?:from|into|onto|branch|base)\\s+[`'\"]?$")
	productJS    = regexp.MustCompile(`^[A-Z][a-z]+\.js$`)

	// A command written inside action text: a fenced block, a code span with a
	// space in it, a "$ " shell line, or "run/execute/invoke ...".
	fencedBlock = regexp.MustCompile("(?s)```(.*?)```")
	codeSpan    = regexp.MustCompile("`([^`\\n]*\\s[^`\\n]*)`")
	shellLine   = regexp.MustCompile(`(?m)^\s*\$ (.+)$`)
	runVerb     = regexp.MustCompile(`(?i)\b(?:run|execute|invoke)\s+(\S(?:[^\n;.]|\.\S)*)`)
	hostFirst   = regexp.MustCompile(`(?i)\.(?:com|io|net|org|dev|app)$`)

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

// askEvidence is what a card says it checked: where a control was seen, which
// commit a file is from, and where a command was dry-run.
type askEvidence struct {
	Observed string
	Files    []StepFile
	DryRun   *StepDryRun
}

// askText is one piece of text the guard reads. A command is a command in
// full; prose may hold commands (commandsIn); uiOnly text is read for UI paths
// alone.
type askText struct {
	field   string
	text    string
	command bool
	uiOnly  bool
}

// commandsIn lists the commands written inside prose.
func commandsIn(text string) []string {
	var out []string
	for _, re := range []*regexp.Regexp{fencedBlock, codeSpan, shellLine, runVerb} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if !blank(m[1]) {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// check runs the human-ask guard for one card or step. subject names the text
// in UI and file errors; home is where observed, files and dry_run live.
func (ev askEvidence) check(subject, home string, texts []askText) error {
	var hasCommand bool
	for _, t := range texts {
		if t.uiOnly {
			continue
		}
		commands := commandsIn(t.text)
		if t.command && !blank(t.text) {
			commands = append(commands, t.text)
		}
		hasCommand = hasCommand || len(commands) > 0
		for _, command := range commands {
			if found := placeholderIn(command); found != "" {
				return &AskError{t.field, "has a placeholder (" + found + "); write the literal the human runs"}
			}
		}
	}
	if err := ev.checkFiles(subject, home, texts); err != nil {
		return err
	}
	for _, t := range texts {
		if uiPath.MatchString(t.text) && blank(ev.Observed) {
			return &AskError{subject, "cites a UI path with no observed; say where and when you saw the control"}
		}
	}
	if hasCommand {
		return ev.checkDryRun(home)
	}
	return nil
}

func placeholderIn(command string) string {
	for _, re := range []*regexp.Regexp{placeholder, placeholderWord} {
		if found := re.FindString(command); found != "" {
			return found
		}
	}
	return strings.TrimSpace(ellipsisText.FindString(command))
}

// repoFiles lists the relative file paths a text names, outside URLs. Absolute
// and home paths belong to the human's own host and stay free.
func repoFiles(text string) []string {
	text = urlText.ReplaceAllString(text, " ")
	var out []string
	for _, m := range repoPath.FindAllStringSubmatchIndex(text, -1) {
		name := text[m[2]:m[3]]
		first, _, _ := strings.Cut(strings.TrimPrefix(name, "./"), "/")
		if hostFirst.MatchString(first) || gitRefBefore.MatchString(text[:m[2]]) {
			continue
		}
		out = append(out, name)
	}
	for _, m := range rootFile.FindAllStringSubmatch(text, -1) {
		if !productJS.MatchString(m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

func (ev askEvidence) checkFiles(subject, home string, texts []askText) error {
	for index, file := range ev.Files {
		at := fmt.Sprintf("%s.files[%d]", home, index)
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
	for _, t := range texts {
		if t.uiOnly {
			continue
		}
		for _, name := range repoFiles(t.text) {
			if !ev.pins(name) {
				return &AskError{subject, "names the file " + name + " without a files entry with repo and a 40-character commit SHA"}
			}
		}
	}
	return nil
}

func (ev askEvidence) pins(name string) bool {
	name = strings.TrimPrefix(name, "./")
	for _, file := range ev.Files {
		if strings.TrimPrefix(file.Path, "./") == name {
			return true
		}
	}
	return false
}

func (ev askEvidence) checkDryRun(home string) error {
	at := home + ".dry_run"
	switch run := ev.DryRun; {
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

// validateAskEvidence runs the human-ask guard checks for one step.
func (step HumanStep) validateAskEvidence(prefix string) error {
	texts := []askText{{field: prefix + ".action", text: step.Action}, {field: prefix + ".command", text: step.Command, command: true}}
	for _, value := range step.Values {
		texts = append(texts, askText{text: value.Source, uiOnly: true})
	}
	return askEvidence{step.Observed, step.Files, step.DryRun}.check(prefix, prefix, texts)
}

// validateActionEvidence runs the same guard over an action card's steps.
func (ask Ask) validateActionEvidence() error {
	texts := make([]askText, len(ask.Steps))
	for index, step := range ask.Steps {
		texts[index] = askText{field: "ask.steps", text: step}
	}
	return askEvidence{ask.Observed, ask.Files, ask.DryRun}.check("ask.steps", "ask", texts)
}

// askEvidenceLines renders what a step proved, placed before "Done when".
func (ev askEvidence) lines() string {
	var out strings.Builder
	if !blank(ev.Observed) {
		fmt.Fprintf(&out, "   - Seen at: %s\n", ev.Observed)
	}
	for _, file := range ev.Files {
		if blank(file.AttachmentID) {
			fmt.Fprintf(&out, "   - File: %s in %s at %s\n", file.Path, file.Repo, file.SHA)
		} else {
			fmt.Fprintf(&out, "   - File: %s from attachment %s\n", file.Path, file.AttachmentID)
		}
	}
	if ev.DryRun != nil {
		fmt.Fprintf(&out, "   - Dry run on %s: %s\n", ev.DryRun.Host, ev.DryRun.Result)
	}
	return out.String()
}

func (step HumanStep) askEvidenceLines() string {
	return askEvidence{step.Observed, step.Files, step.DryRun}.lines()
}
