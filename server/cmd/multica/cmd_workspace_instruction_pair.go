package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var workspaceApplyInstructionPairCmd = &cobra.Command{
	Use:   "apply-instruction-pair <workspace-id>",
	Short: "Conditionally apply workspace context and squad instructions atomically",
	Long: "Applies workspace context and one squad's instructions in a single " +
		"server transaction, only when both expected before-digests still match.",
	Args: exactArgs(1),
	RunE: runWorkspaceApplyInstructionPair,
}

func init() {
	workspaceCmd.AddCommand(workspaceApplyInstructionPairCmd)
	workspaceApplyInstructionPairCmd.Flags().String("squad-id", "", "Squad whose instructions will be applied")
	workspaceApplyInstructionPairCmd.Flags().String("context-file", "", "Read the reviewed workspace context candidate")
	workspaceApplyInstructionPairCmd.Flags().String("instructions-file", "", "Read the reviewed squad instructions candidate")
	workspaceApplyInstructionPairCmd.Flags().String("expected-context-before-digest", "", "sha256 of the current workspace context")
	workspaceApplyInstructionPairCmd.Flags().String("expected-instructions-before-digest", "", "sha256 of the current squad instructions")
	workspaceApplyInstructionPairCmd.Flags().String("expected-context-after-digest", "", "sha256 of the reviewed context candidate")
	workspaceApplyInstructionPairCmd.Flags().String("expected-instructions-after-digest", "", "sha256 of the reviewed instructions candidate")
	workspaceApplyInstructionPairCmd.Flags().String("output", "json", "Output format: table or json")
}

func runWorkspaceApplyInstructionPair(cmd *cobra.Command, args []string) error {
	squadID, _ := cmd.Flags().GetString("squad-id")
	contextPath, _ := cmd.Flags().GetString("context-file")
	instructionsPath, _ := cmd.Flags().GetString("instructions-file")
	contextBefore, _ := cmd.Flags().GetString("expected-context-before-digest")
	instructionsBefore, _ := cmd.Flags().GetString("expected-instructions-before-digest")
	contextAfter, _ := cmd.Flags().GetString("expected-context-after-digest")
	instructionsAfter, _ := cmd.Flags().GetString("expected-instructions-after-digest")
	for name, value := range map[string]string{
		"--squad-id":                            squadID,
		"--expected-context-before-digest":      contextBefore,
		"--expected-instructions-before-digest": instructionsBefore,
		"--expected-context-after-digest":       contextAfter,
		"--expected-instructions-after-digest":  instructionsAfter,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if contextPath == "" || instructionsPath == "" {
		return fmt.Errorf("--context-file and --instructions-file are required")
	}
	for name, digest := range map[string]string{
		"--expected-context-before-digest":      contextBefore,
		"--expected-instructions-before-digest": instructionsBefore,
		"--expected-context-after-digest":       contextAfter,
		"--expected-instructions-after-digest":  instructionsAfter,
	} {
		if !isWellFormedDigestHex(digest) {
			return fmt.Errorf("%s must be exactly 64 hex characters (sha256)", name)
		}
	}

	contextText, err := readInstructionPairCandidate(contextPath, contextAfter, "workspace context")
	if err != nil {
		return err
	}
	instructionsText, err := readInstructionPairCandidate(instructionsPath, instructionsAfter, "squad instructions")
	if err != nil {
		return err
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	body := map[string]any{
		"context":                             contextText,
		"squad_id":                            squadID,
		"instructions":                        instructionsText,
		"expected_context_before_digest":      contextBefore,
		"expected_instructions_before_digest": instructionsBefore,
	}
	var result map[string]any
	path := "/api/workspaces/" + url.PathEscape(args[0]) + "/instruction-pair"
	if err := client.PutJSON(ctx, path, body, &result); err != nil {
		return fmt.Errorf("apply workspace and squad instruction pair: %w", err)
	}

	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	fmt.Printf("Workspace and squad instructions applied atomically: %s / %s\n", args[0], squadID)
	fmt.Printf("Context: %s → %s\n", result["context_before_digest"], result["context_after_digest"])
	fmt.Printf("Instructions: %s → %s\n", result["instructions_before_digest"], result["instructions_after_digest"])
	return nil
}

func readInstructionPairCandidate(path, expectedDigest, label string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s candidate: %w", label, err)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s candidate must be valid UTF-8", label)
	}
	content := string(data)
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != strings.ToLower(expectedDigest) {
		return "", fmt.Errorf("%s candidate digest does not match its reviewed after-digest", label)
	}
	return content, nil
}
