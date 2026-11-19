// Package backfill runs the issuecreatedvia classifier over old issues
// (CHE-6376 S2). The dry run reports what would change; apply writes only
// issue.created_via and a receipt; revert restores from that receipt.
package backfill

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options narrows a run. A zero WorkspaceID scans every workspace.
type Options struct {
	WorkspaceID pgtype.UUID
	BatchSize   int
}

// Sample is one legacy `comment` row with the comment that triggered it.
type Sample struct {
	IssueID          string `json:"issue_id"`
	TriggerCommentID string `json:"trigger_comment_id"`
}

// Report is the dry-run output.
type Report struct {
	Domain               int            `json:"domain"`
	Counts               map[string]int `json:"counts"`
	Legacy               map[string]int `json:"legacy"`
	Unresolved           int            `json:"unresolved"`
	UnresolvedIssueIDs   []string       `json:"unresolved_issue_ids"`
	LegacyCommentSamples []Sample       `json:"legacy_comment_samples"`
	Changed              int            `json:"changed"`
}

// DryRun classifies the domain and writes nothing.
func DryRun(ctx context.Context, pool *pgxpool.Pool, opts Options) (Report, error) {
	return Report{
		Counts:               map[string]int{},
		Legacy:               map[string]int{},
		UnresolvedIssueIDs:   []string{},
		LegacyCommentSamples: []Sample{},
	}, nil
}

// ApplyResult is what Apply did.
type ApplyResult struct {
	Report
	Written int `json:"written"`
	Skipped int `json:"skipped"`
}

// Apply writes the classified values and appends one receipt line per changed
// row to receiptPath.
func Apply(ctx context.Context, pool *pgxpool.Pool, opts Options, receiptPath string) (ApplyResult, error) {
	return ApplyResult{}, nil
}

// RevertResult is what Revert did.
type RevertResult struct {
	Reverted int `json:"reverted"`
	Skipped  int `json:"skipped"`
}

// Revert writes `old` back where the current value still equals `new`.
func Revert(ctx context.Context, pool *pgxpool.Pool, opts Options, receiptPath string) (RevertResult, error) {
	return RevertResult{}, nil
}
