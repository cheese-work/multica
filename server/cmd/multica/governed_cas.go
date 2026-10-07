package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
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

func bodyText(body map[string]any, key string) string {
	s, _ := body[key].(string)
	return s
}
