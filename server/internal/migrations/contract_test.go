package migrations

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// TestParseContractLineFixtures reads the fixture file that N5a's node parser
// also reads, so the two parsers agree on every line.
func TestParseContractLineFixtures(t *testing.T) {
	f, err := os.Open("testdata/contract-lines.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	cases := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		row := sc.Text()
		if row == "" || strings.HasPrefix(row, "#") {
			continue
		}
		fields := strings.Split(row, "\t")
		cases++
		switch fields[0] {
		case "valid":
			if len(fields) != 4 {
				t.Fatalf("bad fixture row %q", row)
			}
			// The line is followed by SQL, as in a real file.
			c, err := ParseContract(fields[1] + "\nSELECT 1;\n")
			if err != nil || c == nil {
				t.Errorf("%q: want a contract, got %v, %v", fields[1], c, err)
				continue
			}
			if got := strings.Join(c.SnapshotTables, ","); got != fields[2] {
				t.Errorf("%q: tables = %q, want %q", fields[1], got, fields[2])
			}
			if c.Restore != fields[3] {
				t.Errorf("%q: restore = %q, want %q", fields[1], c.Restore, fields[3])
			}
		case "invalid":
			if len(fields) != 2 {
				t.Fatalf("bad fixture row %q", row)
			}
			c, err := ParseContract(fields[1] + "\nSELECT 1;\n")
			if err == nil || c != nil {
				t.Errorf("%q: want an error, got %v, %v", fields[1], c, err)
			}
		default:
			t.Fatalf("bad fixture row %q", row)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if cases < 10 {
		t.Fatalf("only %d fixture rows read", cases)
	}
}

func TestParseContractNoContractLine(t *testing.T) {
	for _, sql := range []string{
		"",
		"SELECT 1;\n",
		"-- a comment\nSELECT 1;\n",
		"\n-- multica:contract snapshot=t restore=a.sql\n", // not the first line
		"-- multica:other snapshot=t restore=a.sql\n",
		"  -- multica:contract snapshot=t restore=a.sql\n", // does not start with the marker
	} {
		c, err := ParseContract(sql)
		if c != nil || err != nil {
			t.Errorf("%q: got %v, %v; want no contract", sql, c, err)
		}
	}
}

func TestParseContractAcceptsCRLFAndNoNewline(t *testing.T) {
	for _, sql := range []string{
		"-- multica:contract snapshot=t restore=a.sql\r\nSELECT 1;\r\n",
		"-- multica:contract snapshot=t restore=a.sql",
	} {
		c, err := ParseContract(sql)
		if err != nil || c == nil || c.Restore != "a.sql" {
			t.Errorf("%q: got %v, %v", sql, c, err)
		}
	}
}

func TestTransactionControl(t *testing.T) {
	refused := map[string]string{
		"COMMIT;":                                    "COMMIT",
		"commit":                                     "COMMIT",
		"SELECT 1; BEGIN; SELECT 2;":                 "BEGIN",
		"SELECT 1;\n  rollback ;":                    "ROLLBACK",
		"ROLLBACK TO SAVEPOINT s;":                   "ROLLBACK",
		"SELECT 1; END;":                             "END",
		"/* c */ COMMIT;":                            "COMMIT",
		"-- c\nBEGIN;":                               "BEGIN",
		"SELECT 'x'; /* a /* nested */ b */ COMMIT": "COMMIT",
		"BEGIN TRANSACTION;":                         "BEGIN",
		"ABORT;":                                     "ABORT",
		"START TRANSACTION;":                         "START",
	}
	for sql, want := range refused {
		got, ok := TransactionControl(sql)
		if !ok || got != want {
			t.Errorf("%q: got %q, %v; want %q", sql, got, ok, want)
		}
	}

	allowed := []string{
		"",
		"CREATE TABLE t (id int);",
		"SELECT 'COMMIT;';",
		"SELECT \"commit\" FROM t;",
		"SELECT E'it\\'s; COMMIT;';",
		"SELECT $$ COMMIT; $$;",
		"DO $body$ BEGIN PERFORM 1; END $body$;",
		"CREATE FUNCTION f() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETURN 1; END; $$;",
		"-- COMMIT;\nSELECT 1;",
		"/* BEGIN; */ SELECT 1;",
		"SELECT CASE WHEN true THEN 1 END;",
		"SELECT 1 AS \"end\";\nUPDATE t SET begin_at = now();",
		"CREATE INDEX CONCURRENTLY i ON t (id);", // refused by Postgres in a transaction, not by the scanner
	}
	for _, sql := range allowed {
		if got, ok := TransactionControl(sql); ok {
			t.Errorf("%q: refused as %q; want allowed", sql, got)
		}
	}
}
