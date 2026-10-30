package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/multica-ai/multica/server/internal/cli"
)

// CHE-1300: CLI helpers for the owner-only guarded CAS on agent instructions
// and the ambiguous-failure report shared by all three governed fields.
//
// Digest contract (see server/internal/handler/governed_instruction_owner_cas.go):
// --expected-before-digest is the sha256 of the RAW UTF-8 bytes of the live
// value. The CHE-1286 card manifest strips one terminal LF before hashing; do
// not pass a manifest digest here for a value that ends in LF — the server
// refuses it as stale. Candidate text is read from stdin or a file, never from
// arguments, and is never echoed in errors.

// resolveAgentInstructions reads agent instructions byte for byte. allowInline
// admits --instructions (the pre-existing non-digest behavior); digest mode
// passes false so candidate text never appears in the process arguments.
func resolveAgentInstructions(cmd *cobra.Command, allowInline bool) (string, bool, error) {
	inlineSet := cmd.Flags().Changed("instructions")
	fromStdin, _ := cmd.Flags().GetBool("instructions-stdin")
	filePath, _ := cmd.Flags().GetString("instructions-file")
	fileSet := cmd.Flags().Changed("instructions-file")

	if inlineSet && !allowInline {
		return "", false, errInlineCandidate("instructions")
	}
	sources := 0
	for _, set := range []bool{inlineSet, fromStdin, fileSet} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return "", false, fmt.Errorf("--instructions, --instructions-stdin, and --instructions-file are mutually exclusive")
	}
	if sources == 0 {
		return "", false, nil
	}

	var data []byte
	var err error
	switch {
	case inlineSet:
		data = []byte(mustGetString(cmd, "instructions"))
	case fromStdin:
		data, err = io.ReadAll(cmd.InOrStdin())
	default:
		if filePath == "" {
			return "", false, fmt.Errorf("--instructions-file: path must not be empty")
		}
		data, err = os.ReadFile(filePath)
	}
	if err != nil {
		return "", false, fmt.Errorf("read agent instructions: %w", err)
	}
	if !utf8.Valid(data) {
		return "", false, fmt.Errorf("agent instructions must be valid UTF-8")
	}
	return string(data), true, nil
}

// errInlineCandidate refuses inline candidate text in digest mode: Acceptance 5,
// candidate text never appears in the process arguments.
func errInlineCandidate(field string) error {
	return fmt.Errorf("--expected-before-digest cannot be combined with --%[1]s: inline text would put the candidate in the process arguments; use --%[1]s-stdin or --%[1]s-file", field)
}

func mustGetString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

// agentDigestAllowedFlags are the only update flags a digest-mode agent update
// may carry; connection flags are not body fields.
var agentDigestAllowedFlags = map[string]bool{
	"instructions-stdin": true, "instructions-file": true, "expected-before-digest": true,
	"vote-issue": true, "decision-id": true, "print-hash": true,
	"output": true, "server-url": true, "workspace-id": true, "profile": true, "debug": true,
}

// buildAgentUpdateDigestBody assembles exactly
// {"instructions": ..., "expected_before_digest": ...}; the server refuses any
// other key.
func buildAgentUpdateDigestBody(cmd *cobra.Command) (map[string]any, error) {
	digest := mustGetString(cmd, "expected-before-digest")
	if !isWellFormedDigestHex(digest) {
		return nil, fmt.Errorf("--expected-before-digest must be exactly 64 hex characters (sha256), got %d", len(digest))
	}
	var conflict string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if conflict == "" && !agentDigestAllowedFlags[f.Name] && f.Name != "instructions" {
			conflict = f.Name
		}
	})
	if conflict != "" {
		return nil, fmt.Errorf("--expected-before-digest cannot be combined with --%s; digest-mode updates exactly one field (instructions)", conflict)
	}
	instructions, has, err := resolveAgentInstructions(cmd, false)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, fmt.Errorf("--expected-before-digest requires the new instructions to be set via --instructions-stdin or --instructions-file")
	}
	return map[string]any{"instructions": instructions, "expected_before_digest": digest}, nil
}

// digestWriteError wraps the error from a digest-mode write. A 4xx is a
// definitive refusal: nothing was written. Anything else (transport failure,
// timeout, unreadable response, 5xx) leaves the outcome unknown: report both
// digests so the operator can read the live value and decide. The CLI never
// retries; a blind retry of a write whose outcome is unknown can double-apply
// or mask a concurrent change.
func digestWriteError(op string, err error, candidate, expectedBefore string) error {
	var httpErr *cli.HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode < 500 {
		return fmt.Errorf("%s: %w", op, err)
	}
	// UserMessageError, not a plain wrapper: main prints cli.FormatError, which
	// shows only the root cause's friendly line by default and would drop this
	// warning and the recovery digests.
	return cli.WithUserMessage(fmt.Sprintf("%s: AMBIGUOUS outcome: the write may or may not have been applied; do NOT retry. Read the live value and compare its sha256: %s means not applied, %x means applied",
		op, expectedBefore, sha256.Sum256([]byte(candidate))), err)
}

// CHE-1936: --vote-issue turns a digest-mode body into the CHE-1862
// vote-receipt body that the server's voteApply reads: the same new value plus
// vote_issue, before_sha256 (the --expected-before-digest value) and
// after_sha256, computed here from the exact bytes sent. The server takes the
// vote path only for an agent caller; the votes themselves are comments on the
// vote issue, checked server-side.
var voteIssueUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// checkBindingFlags rejects a malformed --vote-issue, --decision-id or
// --print-hash request before any HTTP call. The two bindings are alternatives.
func checkBindingFlags(cmd *cobra.Command) error {
	voteSet, decisionSet := cmd.Flags().Changed("vote-issue"), cmd.Flags().Changed("decision-id")
	digestSet := cmd.Flags().Changed("expected-before-digest")
	if voteSet && decisionSet {
		return fmt.Errorf("--decision-id and --vote-issue are alternatives; pass one")
	}
	if voteSet {
		if !digestSet {
			return fmt.Errorf("--vote-issue requires --expected-before-digest (the before_sha256 of the vote receipt)")
		}
		if !voteIssueUUID.MatchString(mustGetString(cmd, "vote-issue")) {
			return fmt.Errorf("--vote-issue must be an issue UUID (the server reads votes by issue id)")
		}
	}
	if decisionSet {
		if !digestSet {
			return fmt.Errorf("--decision-id requires --expected-before-digest (the before_sha256 of the decision-bound write)")
		}
		if err := checkDecisionID(cmd); err != nil {
			return err
		}
	}
	if printHashRequested(cmd) && !digestSet {
		return fmt.Errorf("--print-hash requires --expected-before-digest")
	}
	return nil
}

// withBinding returns body unchanged without --vote-issue or --decision-id,
// else the vote-receipt or decision-bound body for field.
func withBinding(cmd *cobra.Command, body map[string]any, field string) map[string]any {
	key := "vote_issue"
	flag := "vote-issue"
	if cmd.Flags().Changed("decision-id") {
		key, flag = "decision_id", "decision-id"
	} else if !cmd.Flags().Changed("vote-issue") {
		return body
	}
	content := bodyText(body, field)
	return map[string]any{
		field:           content,
		key:             mustGetString(cmd, flag),
		"before_sha256": bodyText(body, "expected_before_digest"),
		"after_sha256":  fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
	}
}

// bodyBefore is the before digest of an owner CAS or vote-receipt body.
func bodyBefore(body map[string]any) string {
	if v := bodyText(body, "expected_before_digest"); v != "" {
		return v
	}
	return bodyText(body, "before_sha256")
}

func bodyText(body map[string]any, key string) string {
	s, _ := body[key].(string)
	return s
}
