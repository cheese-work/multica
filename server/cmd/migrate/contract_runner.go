package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/migrations"
)

// defaultSchemaContractsTable sits beside the ledger table, in its schema.
const defaultSchemaContractsTable = "schema_contracts"

// contractsTableIdentifier returns the quoted schema_contracts table that
// belongs to the ledger table `ledger` (which may be schema-qualified).
func contractsTableIdentifier(ledger string) (string, error) {
	name := defaultSchemaContractsTable
	if schema, _, qualified := strings.Cut(ledger, "."); qualified {
		name = schema + "." + defaultSchemaContractsTable
	}
	return quoteQualifiedIdentifier(name)
}

// contractInfo is a contract migration the runner applies in one transaction.
type contractInfo struct {
	*migrations.Contract
}

// classifyContract decides whether the file being run belongs to a contract
// migration and refuses an unsafe one before any statement runs. Up: the file
// is the up file, whose first line is the contract line. Down: the version
// is a contract when its up file is marked or its schema_contracts row exists,
// so a down never deletes a ledger row and leaves the contract row behind.
func classifyContract(ctx context.Context, conn *pgxpool.Conn, opts runOptions, file, version, sql, contractsIdent string) (*contractInfo, error) {
	var contract *migrations.Contract
	if opts.Direction == "up" {
		parsed, err := migrations.ParseContract(sql)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", version, err)
		}
		contract = parsed
	} else {
		upFile := strings.TrimSuffix(file, ".down.sql") + ".up.sql"
		upSQL, err := os.ReadFile(upFile)
		switch {
		case err == nil:
			parsed, perr := migrations.ParseContract(string(upSQL))
			if perr != nil {
				return nil, fmt.Errorf("migration %q: %w", version, perr)
			}
			contract = parsed
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("read up file of %q: %w", version, err)
		}
		var hasRow bool
		if err := conn.QueryRow(ctx,
			fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM %s WHERE version = $1)", contractsIdent), version).Scan(&hasRow); err != nil {
			return nil, fmt.Errorf("check contract row of %q: %w", version, err)
		}
		if contract == nil && hasRow {
			contract = &migrations.Contract{}
		}
	}
	if contract == nil {
		return nil, nil
	}

	if condition, ok := opts.Conditions[version]; ok && condition != nil {
		return nil, fmt.Errorf("contract migration %q (%s) has a registered condition; a contract file always runs", version, opts.Direction)
	}
	if keyword, found := migrations.TransactionControl(sql); found {
		return nil, fmt.Errorf("contract migration %q (%s) holds a %s statement; the runner owns the transaction", version, opts.Direction, keyword)
	}
	return &contractInfo{contract}, nil
}

// applyContract runs a contract migration's file, its ledger change and its
// schema_contracts change in one transaction. Hooks have already run.
func applyContract(ctx context.Context, conn *pgxpool.Conn, opts runOptions, version, sql string, contract *contractInfo, ledgerIdent, contractsIdent string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin contract migration %q: %w", version, err)
	}
	if err := execContract(ctx, tx, opts, version, sql, contract, ledgerIdent, contractsIdent); err != nil {
		// Roll back on a fresh context: ctx may be the reason for the failure.
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return fmt.Errorf("%w (rollback also failed: %w)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit contract migration %q: %w", version, err)
	}
	return nil
}

func execContract(ctx context.Context, tx pgx.Tx, opts runOptions, version, sql string, contract *contractInfo, ledgerIdent, contractsIdent string) error {
	if opts.Direction == "up" {
		if _, err := tx.Exec(ctx, "SELECT set_config('multica.contract_attempt', $1, true)", opts.ContractAttempt); err != nil {
			return fmt.Errorf("set contract attempt for %q: %w", version, err)
		}
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("apply migration %q: %w", version, err)
	}
	if opts.Direction == "up" {
		if _, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s (version) VALUES ($1)", ledgerIdent), version); err != nil {
			return fmt.Errorf("record migration %q: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			fmt.Sprintf("INSERT INTO %s (version, attempt_id, snapshot_tables, restore) VALUES ($1, NULLIF($2, ''), $3, $4)", contractsIdent),
			version, opts.ContractAttempt, strings.Join(contract.SnapshotTables, ","), contract.Restore); err != nil {
			return fmt.Errorf("record contract %q: %w", version, err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE version = $1", ledgerIdent), version); err != nil {
		return fmt.Errorf("record migration %q: %w", version, err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE version = $1", contractsIdent), version); err != nil {
		return fmt.Errorf("delete contract %q: %w", version, err)
	}
	return nil
}
