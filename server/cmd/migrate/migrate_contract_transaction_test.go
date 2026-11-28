package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CHE-7486 N5r — a contract migration (first line `-- multica:contract ...`)
// commits its effects, its ledger row and its schema_contracts row together
// or not at all.

const contractTestLine = "-- multica:contract snapshot=scratch restore=server/cmd/migrate/recovery/scratch_restore.sql\n"

type contractFixture struct {
	*fixture
	version string
	scratch string // schema-qualified, quoted
	upFile  string
	options runOptions
}

// newContractFixture writes one contract migration whose up creates a scratch
// table and whose down drops it.
func newContractFixture(t *testing.T, body string) *contractFixture {
	t.Helper()
	f := newFixture(t)
	dir := t.TempDir()
	version := "900_contract_scratch"
	scratch := pgx.Identifier{f.schema, "scratch"}.Sanitize()
	if body == "" {
		body = contractTestLine + fmt.Sprintf("CREATE TABLE %s (id int);\n", scratch)
	}
	upFile := filepath.Join(dir, version+".up.sql")
	downFile := filepath.Join(dir, version+".down.sql")
	if err := os.WriteFile(upFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(downFile, []byte(fmt.Sprintf("DROP TABLE %s;\n", scratch)), 0o600); err != nil {
		t.Fatal(err)
	}
	return &contractFixture{
		fixture: f,
		version: version,
		scratch: scratch,
		upFile:  upFile,
		options: runOptions{
			Direction:             "up",
			Files:                 []string{upFile},
			SchemaMigrationsTable: f.tableFQN,
			AdvisoryLockKey:       f.lockKey,
		},
	}
}

func (c *contractFixture) down() runOptions {
	o := c.options
	o.Direction = "down"
	o.Files = []string{strings.TrimSuffix(c.upFile, ".up.sql") + ".down.sql"}
	return o
}

// failLedger installs a trigger on the test ledger table that raises for the
// fixture's version on the given event ("INSERT" or "DELETE").
func (c *contractFixture) failLedger(t *testing.T, event string) {
	t.Helper()
	ref := "NEW"
	if event == "DELETE" {
		ref = "OLD"
	}
	ledger := pgx.Identifier{c.schema, "schema_migrations"}.Sanitize()
	fn := pgx.Identifier{c.schema, "fail_ledger"}.Sanitize()
	sql := fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF %[2]s.version = '%[3]s' THEN RAISE EXCEPTION 'ledger refused %[3]s'; END IF;
			RETURN %[2]s;
		END $$;
		CREATE TRIGGER fail_ledger BEFORE %[4]s ON %[5]s
			FOR EACH ROW EXECUTE FUNCTION %[1]s();`, fn, ref, c.version, event, ledger)
	if _, err := c.pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("install ledger trigger: %v", err)
	}
}

func (c *contractFixture) scratchExists(t *testing.T) bool {
	t.Helper()
	return c.tableExists(t, "scratch")
}

func (c *contractFixture) run(t *testing.T, o runOptions) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), raceTestTimeout)
	defer cancel()
	return runMigrations(ctx, c.pool, o)
}

// Red 1: the run fails on the ledger insert; the scratch table must not stay.
func TestContractUpIsAtomicWithLedgerRow(t *testing.T) {
	c := newContractFixture(t, "")
	// The ledger table is created by the runner; create it first through an
	// empty run so the trigger can attach.
	if err := c.run(t, runOptions{Direction: "up", SchemaMigrationsTable: c.tableFQN, AdvisoryLockKey: c.lockKey}); err != nil {
		t.Fatal(err)
	}
	c.failLedger(t, "INSERT")

	if err := c.run(t, c.options); err == nil {
		t.Fatal("run succeeded; want the ledger insert to fail it")
	}
	if c.scratchExists(t) {
		t.Fatal("scratch table exists after a failed contract migration; the window is open")
	}
	if got := c.appliedVersions(t); len(got) != 0 {
		t.Fatalf("ledger rows = %v, want none", got)
	}
}

// Red 2: the run fails on the ledger delete; the down file's effects must not stay.
func TestContractDownIsAtomicWithLedgerRow(t *testing.T) {
	c := newContractFixture(t, "")
	if err := c.run(t, c.options); err != nil {
		t.Fatalf("up: %v", err)
	}
	c.failLedger(t, "DELETE")

	if err := c.run(t, c.down()); err == nil {
		t.Fatal("down succeeded; want the ledger delete to fail it")
	}
	if !c.scratchExists(t) {
		t.Fatal("scratch table dropped although the down failed; the window is open")
	}
	if got := c.appliedVersions(t); len(got) != 1 || got[0] != c.version {
		t.Fatalf("ledger rows = %v, want [%s]", got, c.version)
	}
}

// contractRow reads the fixture's schema_contracts row; found is false when
// there is none. attempt is nil for NULL.
func (c *contractFixture) contractRow(t *testing.T) (found bool, attempt *string, tables, restore string) {
	t.Helper()
	rows, err := c.pool.Query(context.Background(),
		fmt.Sprintf(`SELECT attempt_id, snapshot_tables, restore FROM %s WHERE version = $1`,
			pgx.Identifier{c.schema, "schema_contracts"}.Sanitize()), c.version)
	if err != nil {
		t.Fatalf("read schema_contracts: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return false, nil, "", ""
	}
	if err := rows.Scan(&attempt, &tables, &restore); err != nil {
		t.Fatal(err)
	}
	return true, attempt, tables, restore
}

func TestContractUpRecordsContractRowWithAttempt(t *testing.T) {
	c := newContractFixture(t, "")
	c.options.ContractAttempt = "a1"
	if err := c.run(t, c.options); err != nil {
		t.Fatal(err)
	}
	found, attempt, tables, restore := c.contractRow(t)
	if !found || attempt == nil || *attempt != "a1" {
		t.Fatalf("contract row = found %v attempt %v, want attempt a1", found, attempt)
	}
	if tables != "scratch" || restore != "server/cmd/migrate/recovery/scratch_restore.sql" {
		t.Fatalf("contract row tables=%q restore=%q", tables, restore)
	}
	if !c.scratchExists(t) {
		t.Fatal("scratch table missing after a successful contract migration")
	}
	if got := c.appliedVersions(t); len(got) != 1 || got[0] != c.version {
		t.Fatalf("ledger rows = %v", got)
	}
}

func TestContractUpWithoutAttemptRecordsNull(t *testing.T) {
	c := newContractFixture(t, "")
	if err := c.run(t, c.options); err != nil {
		t.Fatal(err)
	}
	found, attempt, _, _ := c.contractRow(t)
	if !found || attempt != nil {
		t.Fatalf("contract row = found %v attempt %v, want NULL attempt", found, attempt)
	}
}

// The migration's own SQL can read the attempt id (N1a's conversion record does).
func TestContractSQLSeesAttemptSetting(t *testing.T) {
	scratch := ""
	c := newContractFixture(t, "")
	scratch = c.scratch
	body := contractTestLine + fmt.Sprintf(
		"CREATE TABLE %s AS SELECT nullif(current_setting('multica.contract_attempt', true), '') AS attempt;\n", scratch)
	if err := os.WriteFile(c.upFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c.options.ContractAttempt = "a7"
	if err := c.run(t, c.options); err != nil {
		t.Fatal(err)
	}
	var got *string
	if err := c.pool.QueryRow(context.Background(), "SELECT attempt FROM "+scratch).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != "a7" {
		t.Fatalf("attempt seen by the migration = %v, want a7", got)
	}
}

func TestContractDownDeletesContractRow(t *testing.T) {
	c := newContractFixture(t, "")
	c.options.ContractAttempt = "a1"
	if err := c.run(t, c.options); err != nil {
		t.Fatal(err)
	}
	if err := c.run(t, c.down()); err != nil {
		t.Fatalf("down: %v", err)
	}
	if found, _, _, _ := c.contractRow(t); found {
		t.Fatal("schema_contracts row survives the down")
	}
	if c.scratchExists(t) || len(c.appliedVersions(t)) != 0 {
		t.Fatal("down left the scratch table or the ledger row")
	}
}

// A contract row whose up file is gone (or unmarked) is still deleted with its ledger row.
func TestContractDownDeletesRowEvenWhenUpFileIsUnmarked(t *testing.T) {
	c := newContractFixture(t, "")
	if err := c.run(t, c.options); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.upFile); err != nil {
		t.Fatal(err)
	}
	if err := c.run(t, c.down()); err != nil {
		t.Fatalf("down: %v", err)
	}
	if found, _, _, _ := c.contractRow(t); found {
		t.Fatal("schema_contracts row survives the down")
	}
}

func TestContractFileFailureLeavesNothing(t *testing.T) {
	c := newContractFixture(t, "")
	body := contractTestLine + fmt.Sprintf("CREATE TABLE %s (id int);\nSELECT 1/0;\n", c.scratch)
	if err := os.WriteFile(c.upFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.run(t, c.options); err == nil {
		t.Fatal("run succeeded")
	}
	if c.scratchExists(t) || len(c.appliedVersions(t)) != 0 {
		t.Fatal("failed contract file left effects or a ledger row")
	}
	if found, _, _, _ := c.contractRow(t); found {
		t.Fatal("failed contract file left a contract row")
	}
}

// Every refusal happens before any statement: no hook, no table, no ledger row.
func TestContractRefusedBeforeAnyStatement(t *testing.T) {
	scratchDDL := func(c *contractFixture) string { return fmt.Sprintf("CREATE TABLE %s (id int);\n", c.scratch) }
	cases := []struct {
		name string
		body func(c *contractFixture) string
		cond bool
		want string
	}{
		{"malformed line", func(c *contractFixture) string {
			return "-- multica:contract snapshot=Scratch restore=a.sql\n" + scratchDDL(c)
		}, false, "malformed contract line"},
		{"missing restore", func(c *contractFixture) string {
			return "-- multica:contract snapshot=scratch\n" + scratchDDL(c)
		}, false, "malformed contract line"},
		{"COMMIT", func(c *contractFixture) string { return contractTestLine + scratchDDL(c) + "COMMIT;\n" }, false, "COMMIT"},
		{"BEGIN", func(c *contractFixture) string { return contractTestLine + "BEGIN;\n" + scratchDDL(c) }, false, "BEGIN"},
		{"ROLLBACK", func(c *contractFixture) string { return contractTestLine + scratchDDL(c) + "ROLLBACK;\n" }, false, "ROLLBACK"},
		{"END", func(c *contractFixture) string { return contractTestLine + scratchDDL(c) + "END;\n" }, false, "END"},
		{"registered condition", func(c *contractFixture) string { return contractTestLine + scratchDDL(c) }, true, "registered condition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newContractFixture(t, "")
			if err := os.WriteFile(c.upFile, []byte(tc.body(c)), 0o600); err != nil {
				t.Fatal(err)
			}
			hookRan := false
			c.options.Hooks = map[string]preMigrationHook{c.version: func(context.Context, *pgxpool.Pool) error {
				hookRan = true
				return nil
			}}
			if tc.cond {
				c.options.Conditions = map[string]migrationCondition{c.version: func(context.Context, *pgxpool.Conn) (bool, string, error) {
					return true, "", nil
				}}
			}
			err := c.run(t, c.options)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if hookRan {
				t.Fatal("pre-migration hook ran before the refusal")
			}
			if c.scratchExists(t) || len(c.appliedVersions(t)) != 0 {
				t.Fatal("refused file left effects or a ledger row")
			}
		})
	}
}

// A down file with transaction control is refused too, before any statement.
func TestContractDownRefusedWithTransactionControl(t *testing.T) {
	c := newContractFixture(t, "")
	if err := c.run(t, c.options); err != nil {
		t.Fatal(err)
	}
	downFile := strings.TrimSuffix(c.upFile, ".up.sql") + ".down.sql"
	if err := os.WriteFile(downFile, []byte(fmt.Sprintf("DROP TABLE %s;\nCOMMIT;\n", c.scratch)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.run(t, c.down()); err == nil || !strings.Contains(err.Error(), "COMMIT") {
		t.Fatalf("err = %v, want a COMMIT refusal", err)
	}
	if !c.scratchExists(t) || len(c.appliedVersions(t)) != 1 {
		t.Fatal("refused down changed the schema or the ledger")
	}
}

// A contract file cannot hold CREATE INDEX CONCURRENTLY; Postgres refuses it
// inside the transaction and nothing stays.
func TestContractFileWithConcurrentIndexFailsWithoutEffects(t *testing.T) {
	c := newContractFixture(t, "")
	body := contractTestLine + fmt.Sprintf("CREATE TABLE %s (id int);\nCREATE INDEX CONCURRENTLY scratch_id ON %s (id);\n", c.scratch, c.scratch)
	if err := os.WriteFile(c.upFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.run(t, c.options); err == nil {
		t.Fatal("run succeeded")
	}
	if c.scratchExists(t) || len(c.appliedVersions(t)) != 0 {
		t.Fatal("failed contract file left effects or a ledger row")
	}
}

// A file without the contract line runs as before, CREATE INDEX CONCURRENTLY included.
func TestUnmarkedFileWithConcurrentIndexStillApplies(t *testing.T) {
	c := newContractFixture(t, "")
	body := fmt.Sprintf("CREATE TABLE %s (id int);\n", c.scratch)
	if err := os.WriteFile(c.upFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(filepath.Dir(c.upFile), "901_scratch_index.up.sql")
	if err := os.WriteFile(idx, []byte(fmt.Sprintf("CREATE INDEX CONCURRENTLY scratch_id ON %s (id);\n", c.scratch)), 0o600); err != nil {
		t.Fatal(err)
	}
	c.options.Files = []string{c.upFile, idx}
	if err := c.run(t, c.options); err != nil {
		t.Fatal(err)
	}
	if got := c.appliedVersions(t); len(got) != 2 {
		t.Fatalf("ledger rows = %v, want both versions", got)
	}
	if found, _, _, _ := c.contractRow(t); found {
		t.Fatal("unmarked file got a contract row")
	}
}

// The runner creates schema_contracts beside the ledger, in the same schema.
func TestSchemaContractsTableBesideLedger(t *testing.T) {
	f := newFixture(t)
	if err := runMigrations(context.Background(), f.pool, f.opts()); err != nil {
		t.Fatal(err)
	}
	if !f.tableExists(t, "schema_contracts") {
		t.Fatal("schema_contracts missing from the ledger's schema")
	}
}
