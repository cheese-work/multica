package decisiontx

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Owner-script guard (CHE-7138 S7a). An agent must not hand a member a shell
// command or script to run: the allowed ways are `multica config propose`,
// `multica decision propose <type>`, a bot permission, or a link with no run
// cue. Each rule below is lexical or typed, so the server decides with no
// reader. Fixtures F-1 to F-8 live in ownerscripttest and are the controls.
//
//	R1  a fence whose info string is a shell language
//	R2  a fence with no info string whose first non-blank line starts with a command
//	R3  a non-empty human_steps[].command
//	R4  a steps[] entry or human_steps[].action that starts with a command
//	R5  a script attachment (extension, or "#!" first)
//	R6  a code line (any fence) or inline code span that starts with a command
//	R7  a prose sentence holding a run cue, in a message that holds a command
//	R8  a run cue in a message that links to, or follows within 30 minutes
//	    from the same author, a message with R1 to R7 features
//
// OwnerScriptRuleVersion changes whenever a rule or a list below changes, so an
// event says which rule set judged it.
const OwnerScriptRuleVersion = 1

// maxOwnerScriptHMACs caps cmd_hmacs on one event.
const maxOwnerScriptHMACs = 20

// OwnerScriptMessage is what the guard reads from one message. Each Texts entry
// is markdown read on its own, so an unclosed fence in one field never swallows
// the next.
type OwnerScriptMessage struct {
	Texts []string
	// Paths names each Texts entry for a refusal; a missing name reads "text[i]".
	Paths       []string
	Steps       []string
	HumanSteps  []OwnerScriptStep
	Attachments []OwnerScriptAttachment
	// Related is the R8 context, resolved by the caller from stored events.
	Related bool
}

type OwnerScriptStep struct{ Action, Command string }

// OwnerScriptAttachment is a file name and its first bytes (a "#!" line is read).
type OwnerScriptAttachment struct{ Name, Head string }

// OwnerScriptFeatures are the rule hits (R1 to R8, in order) and the commands
// found, normalized and without duplicates.
type OwnerScriptFeatures struct {
	Hits     []string
	Commands []string
	// Where names the field of each hit, "R6 ask.agent_did", for a refusal.
	Where []string
	// RunCue is true when a prose sentence holds a run cue: only then does the
	// caller need the R8 context.
	RunCue bool
}

// Primary reports a hit of R1 to R7: the features that make a later run cue
// (R8) count.
func (features OwnerScriptFeatures) Primary() bool {
	for _, hit := range features.Hits {
		if hit != "R8" {
			return true
		}
	}
	return false
}

var (
	shellLanguages = setOf("sh", "bash", "zsh", "shell", "console", "powershell", "pwsh", "ps1", "bat", "cmd")
	scriptSuffixes = []string{".sh", ".bash", ".zsh", ".ps1", ".bat", ".cmd", ".py", ".sql"}

	// commandWords is the fixed token list; matching is exact and lower case.
	commandWords = setOf("sudo", "bash", "sh", "zsh", "fish", "curl", "wget", "git", "gh", "docker", "docker-compose", "podman",
		"systemctl", "journalctl", "service", "psql", "sqlite3", "redis-cli", "python", "python3", "pip", "pip3", "npm", "npx", "pnpm",
		"yarn", "bun", "node", "make", "cargo", "kubectl", "helm", "terraform", "ansible", "ssh", "scp", "rsync", "chmod", "chown",
		"rm", "mv", "cp", "cat", "echo", "export", "apt", "apt-get", "brew", "tailscale", "openssl", "tar", "unzip", "sed", "awk",
		"grep", "xargs", "kill", "pkill", "multica")
	// ambiguousWords are command words that are also plain English. In prose
	// they count only straight after the run cue ("run make deploy").
	ambiguousWords = setOf("make", "cat", "echo", "export", "mv", "cp", "node", "kill", "tar", "service", "bun", "grep", "sed", "awk")

	runCues       = setOf("run", "running", "execute", "executing", "paste", "type", "chạy", "dán", "gõ")
	runCuePhrases = []string{"open a terminal", "open the terminal", "open terminal", "mở terminal"}

	fenceOpen   = regexp.MustCompile("^\\s{0,3}(`{3,}|~{3,})\\s*(\\S*)")
	inlineSpan  = regexp.MustCompile("`([^`\\n]+)`")
	mdLink      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	sentenceEnd = regexp.MustCompile(`[.!?]+(?:\s+|$)|\n+`)
)

func setOf(words ...string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}

func isScriptPath(word string) bool {
	if strings.HasPrefix(word, "./") || strings.HasPrefix(word, "../") || strings.HasPrefix(word, "~/") {
		return true
	}
	return !strings.Contains(word, "://") && slices.ContainsFunc([]string{".sh", ".py", ".ps1"}, func(suffix string) bool { return strings.HasSuffix(word, suffix) })
}

// stripPrompt drops the optional "$ " or "# " in front of a command.
func stripPrompt(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "$ ") || strings.HasPrefix(line, "# ") {
		line = strings.TrimSpace(line[2:])
	}
	return line
}

func normalizeCommand(line string) string { return strings.Join(strings.Fields(line), " ") }

// startsWithCommand reports whether the first word is a command word or a
// script path.
func startsWithCommand(line string) bool {
	words := strings.Fields(stripPrompt(line))
	return len(words) > 0 && (commandWords[words[0]] || isScriptPath(words[0]))
}

// isCommandLine is the code-line test of R2 and R6. A single word that is only
// a command name ("docker", "make") is a name, not a command, unless it is a
// script path.
func isCommandLine(line string) bool {
	words := strings.Fields(stripPrompt(line))
	if len(words) == 0 {
		return false
	}
	return isScriptPath(words[0]) || commandWords[words[0]] && len(words) > 1
}

// OwnerScriptKey derives the HMAC key for cmd_hmacs from the server secret
// with HKDF, so the stored hashes cannot be tested by anyone without it.
func OwnerScriptKey(secret []byte) []byte {
	key, err := hkdf.Key(sha256.New, secret, nil, "multica owner-script guard cmd_hmacs v1", sha256.Size)
	if err != nil {
		panic("owner script key: " + err.Error()) // only for a length beyond 255 hashes
	}
	return key
}

// OwnerScriptCmdHMACs returns the hex HMAC-SHA-256 of each distinct normalized
// command, at most 20.
func OwnerScriptCmdHMACs(key []byte, commands []string) []string {
	hashes := []string{}
	for _, command := range commands {
		command = normalizeCommand(command)
		if command == "" {
			continue
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(command))
		hash := hex.EncodeToString(mac.Sum(nil))
		if !slices.Contains(hashes, hash) {
			hashes = append(hashes, hash)
		}
		if len(hashes) == maxOwnerScriptHMACs {
			break
		}
	}
	return hashes
}

// OwnerScriptMessageFromAsk reads every person-facing field of an ask, and the
// footer. Agent-side fields (probes, agent_commands, state claims) are not
// shown to the person as steps, so they are not read.
func OwnerScriptMessageFromAsk(ask Ask, footer string) OwnerScriptMessage {
	message := OwnerScriptMessage{Steps: ask.Steps}
	add := func(name, text string) {
		if strings.TrimSpace(text) != "" {
			message.Texts = append(message.Texts, text)
			message.Paths = append(message.Paths, name)
		}
	}
	for _, field := range []struct{ name, text string }{
		{"context_eli10", ask.ContextELI10}, {"agent_did", ask.AgentDid}, {"decision", ask.Decision},
		{"stakes_if_wrong", ask.StakesIfWrong}, {"recommendation", ask.Recommendation},
		{"default_if_no_reply", ask.DefaultIfNoReply}, {"after_done", ask.AfterDone}, {"subject", ask.Subject},
		{"why_human", ask.WhyHuman}, {"done_when", ask.DoneWhen}, {"footer", footer},
	} {
		add("ask."+field.name, field.text)
	}
	for i, step := range ask.Steps {
		add(fmt.Sprintf("ask.steps[%d]", i), step)
	}
	for i, option := range ask.Options {
		base := fmt.Sprintf("ask.options[%d].", i)
		add(base+"label", option.Label)
		add(base+"what_happens", option.WhatHappens)
		add(base+"cost_or_time", option.CostOrTime)
		for j, pro := range option.Pros {
			add(fmt.Sprintf("%spros[%d]", base, j), pro)
		}
		for j, con := range option.Cons {
			add(fmt.Sprintf("%scons[%d]", base, j), con)
		}
	}
	for i, step := range ask.HumanSteps {
		add(fmt.Sprintf("ask.human_steps[%d].action", i), step.Action)
		add(fmt.Sprintf("ask.human_steps[%d].verify_done", i), step.VerifyDone)
		message.HumanSteps = append(message.HumanSteps, OwnerScriptStep{Action: step.Action, Command: step.Command})
	}
	return message
}

// ExtractOwnerScript applies R1 to R8 to one message.
func ExtractOwnerScript(message OwnerScriptMessage) OwnerScriptFeatures {
	hit := map[string]bool{}
	where := map[string]string{}
	var commands []string
	addCommand := func(command string) {
		if command = normalizeCommand(command); command != "" && !slices.Contains(commands, command) {
			commands = append(commands, command)
		}
	}
	cue, proseCommand := false, false
	for i, text := range message.Texts {
		name := fmt.Sprintf("text[%d]", i)
		if i < len(message.Paths) {
			name = message.Paths[i]
		}
		local := map[string]bool{}
		prose := scanCode(text, local, addCommand)
		for _, sentence := range sentences(prose) {
			sentenceCue, command := scanSentence(sentence)
			cue = cue || sentenceCue
			if sentenceCue && command != "" {
				proseCommand = true
				where["R7"] = firstWhere(where["R7"], name)
				addCommand(command)
			}
		}
		for rule := range local {
			hit[rule] = true
			where[rule] = firstWhere(where[rule], name)
		}
	}
	for i, step := range message.HumanSteps {
		if strings.TrimSpace(step.Command) != "" {
			hit["R3"] = true
			where["R3"] = firstWhere(where["R3"], fmt.Sprintf("ask.human_steps[%d].command", i))
			addCommand(step.Command)
		}
	}
	for i, step := range message.Steps {
		if startsWithCommand(step) {
			hit["R4"] = true
			where["R4"] = firstWhere(where["R4"], fmt.Sprintf("ask.steps[%d]", i))
			addCommand(stripPrompt(step))
		}
	}
	for i, step := range message.HumanSteps {
		if startsWithCommand(step.Action) {
			hit["R4"] = true
			where["R4"] = firstWhere(where["R4"], fmt.Sprintf("ask.human_steps[%d].action", i))
			addCommand(stripPrompt(step.Action))
		}
	}
	for _, attachment := range message.Attachments {
		name := strings.ToLower(path.Base(attachment.Name))
		if slices.ContainsFunc(scriptSuffixes, func(suffix string) bool { return strings.HasSuffix(name, suffix) }) || strings.HasPrefix(attachment.Head, "#!") {
			hit["R5"] = true
			where["R5"] = firstWhere(where["R5"], "attachment "+attachment.Name)
		}
	}
	if cue && (proseCommand || hit["R1"] || hit["R2"] || hit["R3"] || hit["R4"] || hit["R5"] || hit["R6"]) {
		hit["R7"] = true
	}
	if cue && message.Related {
		hit["R8"] = true
	}
	var features OwnerScriptFeatures
	for _, rule := range []string{"R1", "R2", "R3", "R4", "R5", "R6", "R7", "R8"} {
		if hit[rule] {
			features.Hits = append(features.Hits, rule)
			if where[rule] != "" {
				features.Where = append(features.Where, rule+" "+where[rule])
			}
		}
	}
	features.RunCue = cue
	features.Commands = commands
	return features
}

func firstWhere(have, name string) string {
	if have != "" {
		return have
	}
	return name
}

// scanCode walks the fences and inline spans of one markdown text, records
// R1, R2 and R6, and returns the text outside fences with inline code kept as
// plain words (so "please run `sudo ls`" is one sentence).
func scanCode(text string, hit map[string]bool, addCommand func(string)) string {
	var prose strings.Builder
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		open := fenceOpen.FindStringSubmatch(lines[i])
		if open == nil {
			scanSpans(lines[i], hit, addCommand)
			prose.WriteString(lines[i])
			prose.WriteByte('\n')
			continue
		}
		info := strings.ToLower(open[2])
		var body []string
		for i++; i < len(lines); i++ {
			if closing := strings.TrimSpace(lines[i]); strings.HasPrefix(closing, open[1]) && strings.Trim(closing, open[1][:1]) == "" {
				break
			}
			body = append(body, lines[i])
		}
		if shellLanguages[info] {
			hit["R1"] = true
		}
		first := true
		for _, line := range body {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if first && info == "" && isCommandLine(line) {
				hit["R2"] = true
			}
			first = false
			if isCommandLine(line) {
				hit["R6"] = true
				addCommand(stripPrompt(line))
			}
		}
	}
	return prose.String()
}

func scanSpans(line string, hit map[string]bool, addCommand func(string)) {
	for _, span := range inlineSpan.FindAllStringSubmatch(line, -1) {
		if isCommandLine(span[1]) {
			hit["R6"] = true
			addCommand(stripPrompt(span[1]))
		}
	}
}

func sentences(prose string) []string {
	prose = mdLink.ReplaceAllString(prose, "$1")
	prose = strings.ReplaceAll(prose, "`", "")
	return sentenceEnd.Split(prose, -1)
}

const wordEdge = "\"'()[]{},;:!?*_"

// scanSentence reports whether a sentence holds a run cue and, when it does,
// the command the sentence names after the cue (from the command word to the
// end of the sentence).
func scanSentence(sentence string) (cue bool, command string) {
	words := strings.Fields(sentence)
	lower := strings.ToLower(sentence)
	cueAt := -1
	for i, word := range words {
		if runCues[strings.ToLower(strings.Trim(word, wordEdge+"."))] {
			cueAt = i
			break
		}
	}
	phrase := slices.ContainsFunc(runCuePhrases, func(p string) bool { return strings.Contains(lower, p) })
	if cueAt < 0 && !phrase {
		return false, ""
	}
	from := cueAt + 1
	if cueAt < 0 {
		from = 0
	}
	for i := from; i < len(words); i++ {
		word := strings.Trim(words[i], wordEdge)
		word = strings.TrimRightFunc(word, func(r rune) bool { return r == '.' })
		if isScriptPath(word) || commandWords[word] && (!ambiguousWords[word] || i == from) {
			return true, strings.Join(words[i:], " ")
		}
	}
	return true, ""
}
