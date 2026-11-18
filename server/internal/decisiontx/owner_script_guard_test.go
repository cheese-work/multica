package decisiontx

import (
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx/ownerscripttest"
)

func TestExtractOwnerScriptFixtures(test *testing.T) {
	for _, fixture := range ownerscripttest.All {
		test.Run(fixture.ID, func(test *testing.T) {
			got := ExtractOwnerScript(OwnerScriptMessage{Texts: []string{fixture.Text}, Related: fixture.Related})
			if !slices.Equal(got.Hits, fixture.Hits) {
				test.Fatalf("hits = %v, want %v", got.Hits, fixture.Hits)
			}
		})
	}
}

func TestExtractOwnerScriptRules(test *testing.T) {
	for _, tc := range []struct {
		name string
		msg  OwnerScriptMessage
		want []string
	}{
		{"R1 bash fence", OwnerScriptMessage{Texts: []string{"```bash\nls\n```"}}, []string{"R1"}},
		{"R1 every shell language", OwnerScriptMessage{Texts: []string{"```pwsh\nGet-Date\n```"}}, []string{"R1"}},
		{"R2 bare fence with a command first", OwnerScriptMessage{Texts: []string{"```\n\n$ docker ps\n```"}}, []string{"R2", "R6"}},
		{"bare fence of output is clean", OwnerScriptMessage{Texts: []string{"```\nok: 3 passed\n```"}}, nil},
		{"R3 human step command", OwnerScriptMessage{HumanSteps: []OwnerScriptStep{{Action: "Open Settings", Command: "anything"}}}, []string{"R3"}},
		{"blank human step command is clean", OwnerScriptMessage{HumanSteps: []OwnerScriptStep{{Action: "Open Settings, Secrets, New secret; paste the token you created", Command: "  "}}}, nil},
		{"R4 step starting with a command", OwnerScriptMessage{Steps: []string{"bash deploy.sh"}}, []string{"R4"}},
		{"R4 human step action starting with a command", OwnerScriptMessage{HumanSteps: []OwnerScriptStep{{Action: "git push origin main"}}}, []string{"R4"}},
		{"R5 script by extension", OwnerScriptMessage{Attachments: []OwnerScriptAttachment{{Name: "fix.SH"}}}, []string{"R5"}},
		{"R5 script by shebang", OwnerScriptMessage{Attachments: []OwnerScriptAttachment{{Name: "notes.txt", Head: "#!/bin/sh\nrm -rf x"}}}, []string{"R5"}},
		{"plain attachment is clean", OwnerScriptMessage{Attachments: []OwnerScriptAttachment{{Name: "shot.png", Head: "\x89PNG"}}}, nil},
		{"R6 inline command span", OwnerScriptMessage{Texts: []string{"then `git push` it"}}, []string{"R6"}},
		{"inline paths and names are clean", OwnerScriptMessage{Texts: []string{"see `server/x.go`, `--flag`, `CHE-1` and `docker`"}}, nil},
		{"R6 fence of any info string", OwnerScriptMessage{Texts: []string{"```json\nsudo ls\n```"}}, []string{"R6"}},
		{"R6 script path span", OwnerScriptMessage{Texts: []string{"`./deploy.sh`"}}, []string{"R6"}},
		{"R7 cue and command in prose", OwnerScriptMessage{Texts: []string{"Please run make deploy."}}, []string{"R7"}},
		{"R7 Vietnamese cue", OwnerScriptMessage{Texts: []string{"Anh chạy curl https://x.example nhé."}}, []string{"R7"}},
		{"cue with no command is clean", OwnerScriptMessage{Texts: []string{"Open Settings, Secrets, New secret; paste the token you created."}}, nil},
		{"an ambiguous word after a cue is clean", OwnerScriptMessage{Texts: []string{"Please run it again to make sure."}}, nil},
		{"command word without a cue is clean", OwnerScriptMessage{Texts: []string{"The deploy used git and docker."}}, nil},
		{"R8 needs context", OwnerScriptMessage{Texts: []string{"please run the commands in the link"}}, nil},
		{"R8 with context", OwnerScriptMessage{Texts: []string{"please run the commands in the link"}, Related: true}, []string{"R8"}},
		{"context without a cue is clean", OwnerScriptMessage{Texts: []string{"Details are in the link."}, Related: true}, nil},
		{"markdown link target is not read", OwnerScriptMessage{Texts: []string{"Please run [the check](https://x.example/git/docker)."}}, nil},
	} {
		test.Run(tc.name, func(test *testing.T) {
			if got := ExtractOwnerScript(tc.msg).Hits; !slices.Equal(got, tc.want) {
				test.Fatalf("hits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOwnerScriptCommandsAreNormalizedAndKeyedHashesMatch(test *testing.T) {
	key := OwnerScriptKey([]byte("secret"))
	var first []string
	for _, id := range []string{"F-1", "F-2", "F-3", "F-6", "F-7"} {
		fixture := ownerscripttest.ByID(id)
		got := ExtractOwnerScript(OwnerScriptMessage{Texts: []string{fixture.Text}})
		if !slices.Equal(got.Commands, []string{ownerscripttest.Command}) {
			test.Fatalf("%s commands = %q", id, got.Commands)
		}
		hmacs := OwnerScriptCmdHMACs(key, got.Commands)
		if len(hmacs) != 1 || len(hmacs[0]) != 64 {
			test.Fatalf("%s hmacs = %q", id, hmacs)
		}
		if first == nil {
			first = hmacs
		} else if !slices.Equal(first, hmacs) {
			test.Fatalf("%s hash differs from F-1", id)
		}
	}
	if other := OwnerScriptCmdHMACs(OwnerScriptKey([]byte("another")), []string{ownerscripttest.Command}); other[0] == first[0] {
		test.Fatal("hash did not depend on the key")
	}
	if got := OwnerScriptCmdHMACs(key, []string{"  sudo   ls\t-l ", "sudo ls -l"}); len(got) != 1 {
		test.Fatalf("equal commands after normalizing must give one hash, got %d", len(got))
	}
	many := make([]string, 30)
	for i := range many {
		many[i] = "echo " + string(rune('a'+i))
	}
	if got := OwnerScriptCmdHMACs(key, many); len(got) != 20 {
		test.Fatalf("hashes = %d, want at most 20", len(got))
	}
}

func TestOwnerScriptMessageFromAskReadsEveryAskField(test *testing.T) {
	ask := Ask{ContextELI10: "context", Steps: []string{"bash x.sh"}, HumanSteps: []HumanStep{{Action: "act", Command: "ls", VerifyDone: "check"}},
		Options: []AskOption{{Label: "A", WhatHappens: "happens", Pros: []string{"p"}, Cons: []string{"c"}, CostOrTime: "1 h"}}}
	msg := OwnerScriptMessageFromAsk(ask, "footer text")
	if !slices.Contains(msg.Texts, "context") || !slices.Contains(msg.Texts, "footer text") || !slices.Contains(msg.Texts, "happens") || !slices.Contains(msg.Texts, "check") {
		test.Fatalf("texts = %q", msg.Texts)
	}
	if !slices.Equal(msg.Steps, ask.Steps) || len(msg.HumanSteps) != 1 || msg.HumanSteps[0].Command != "ls" {
		test.Fatalf("steps = %+v / %+v", msg.Steps, msg.HumanSteps)
	}
}

func TestOwnerScriptFeaturesNameTheFieldOfEachHit(test *testing.T) {
	ask := Ask{ContextELI10: "plain", AgentDid: "```text\nsudo ls -l\n```", Steps: []string{"bash x.sh"},
		HumanSteps: []HumanStep{{Action: "Open Settings", Command: "ls"}}}
	got := ExtractOwnerScript(OwnerScriptMessageFromAsk(ask, ""))
	want := []string{"R3 ask.human_steps[0].command", "R4 ask.steps[0]", "R6 ask.agent_did"}
	if !slices.Equal(got.Where, want) {
		test.Fatalf("where = %q, want %q", got.Where, want)
	}
	if !got.RunCue {
		// no cue here: the flag must stay false
		return
	}
	test.Fatal("RunCue is set without a run cue")
}

func TestOwnerScriptFeaturesReportRunCue(test *testing.T) {
	for text, want := range map[string]bool{"please run it": true, "Open a terminal.": true, "Details are in the link.": false} {
		if got := ExtractOwnerScript(OwnerScriptMessage{Texts: []string{text}}).RunCue; got != want {
			test.Fatalf("RunCue(%q) = %v, want %v", text, got, want)
		}
	}
}
