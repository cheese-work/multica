package migrations

import (
	"fmt"
	"regexp"
	"strings"
)

// ContractMarker opens the first line of an up file that declares itself a
// contract migration:
//
//	-- multica:contract snapshot=<t1>[,<t2>] restore=<repo path>
//
// The runner applies such a file, its ledger row and its schema_contracts row
// in one transaction. The node parser of the controller reads the same
// fixtures (testdata/contract-lines.txt), so the two parsers agree.
const ContractMarker = "-- multica:contract"

var (
	contractTableName   = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	contractRestorePath = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// Contract is a parsed contract line.
type Contract struct {
	SnapshotTables []string
	Restore        string
}

// ParseContract reads the first line of an up file. It returns nil, nil when
// the line does not start with ContractMarker. A first line that does start
// with it and does not parse is an error, never "no contract".
func ParseContract(sql string) (*Contract, error) {
	line, _, _ := strings.Cut(sql, "\n")
	line = strings.TrimSuffix(line, "\r")
	if !strings.HasPrefix(line, ContractMarker) {
		return nil, nil
	}
	c, err := parseContractLine(line)
	if err != nil {
		return nil, fmt.Errorf("malformed contract line %q: %w", line, err)
	}
	return c, nil
}

func parseContractLine(line string) (*Contract, error) {
	rest := strings.TrimPrefix(line, ContractMarker)
	// Exactly one space between the marker and each field: the node parser
	// uses the same split, so a line either parses in both or in neither.
	fields := strings.Split(rest, " ")
	if len(fields) != 3 || fields[0] != "" {
		return nil, fmt.Errorf("want %q snapshot=<tables> restore=<path>", ContractMarker)
	}
	tablesValue, ok := strings.CutPrefix(fields[1], "snapshot=")
	if !ok {
		return nil, fmt.Errorf("first field must be snapshot=<tables>")
	}
	restore, ok := strings.CutPrefix(fields[2], "restore=")
	if !ok {
		return nil, fmt.Errorf("second field must be restore=<path>")
	}

	tables := strings.Split(tablesValue, ",")
	seen := make(map[string]bool, len(tables))
	for _, table := range tables {
		if !contractTableName.MatchString(table) {
			return nil, fmt.Errorf("table name %q does not match %s", table, contractTableName)
		}
		if seen[table] {
			return nil, fmt.Errorf("table %q listed twice", table)
		}
		seen[table] = true
	}

	if !contractRestorePath.MatchString(restore) || strings.HasPrefix(restore, "/") {
		return nil, fmt.Errorf("restore path %q must be a relative repo path", restore)
	}
	for _, segment := range strings.Split(restore, "/") {
		if segment == "" || segment == ".." {
			return nil, fmt.Errorf("restore path %q has an empty or .. segment", restore)
		}
	}
	return &Contract{SnapshotTables: tables, Restore: restore}, nil
}

// TransactionControl reports the first statement of sql that is a
// transaction-control statement (BEGIN, COMMIT, ROLLBACK, END, and their
// synonyms ABORT and START), as an upper-case keyword. Comments, quoted
// identifiers, string literals and dollar-quoted bodies are skipped, so a DO
// block with its own BEGIN ... END is not reported. The runner refuses a
// contract file that has one, because the runner owns the transaction.
//
// A statement inside a SQL-standard function body (BEGIN ATOMIC ... END) that
// starts with one of these words is reported too; the refusal is safe.
func TransactionControl(sql string) (string, bool) {
	atStart := true // next word starts a statement
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '-' && strings.HasPrefix(sql[i:], "--"):
			i = skipLineComment(sql, i)
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			i = skipBlockComment(sql, i)
		case c == '\'':
			i = skipQuoted(sql, i, '\'', i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && isWordStart(sql, i-1))
			atStart = false
		case c == '"':
			i = skipQuoted(sql, i, '"', false)
			atStart = false
		case c == '$':
			if end, ok := skipDollarQuoted(sql, i); ok {
				i = end
				atStart = false
			} else {
				i++
				atStart = false
			}
		case c == ';':
			atStart = true
			i++
		case isWordByte(c):
			j := i
			for j < len(sql) && isWordByte(sql[j]) {
				j++
			}
			if atStart {
				switch word := strings.ToUpper(sql[i:j]); word {
				case "BEGIN", "COMMIT", "ROLLBACK", "END", "ABORT", "START":
					return word, true
				}
			}
			atStart = false
			i = j
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		default:
			atStart = false
			i++
		}
	}
	return "", false
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// isWordStart reports whether the byte at i begins a word (so an E before a
// quote is the escape-string prefix, not the tail of an identifier).
func isWordStart(sql string, i int) bool {
	return i == 0 || !isWordByte(sql[i-1])
}

func skipLineComment(sql string, i int) int {
	if end := strings.IndexByte(sql[i:], '\n'); end >= 0 {
		return i + end + 1
	}
	return len(sql)
}

// skipBlockComment skips a /* */ comment; Postgres block comments nest.
func skipBlockComment(sql string, i int) int {
	depth := 0
	for i < len(sql) {
		switch {
		case strings.HasPrefix(sql[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(sql[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(sql)
}

// skipQuoted skips a '...' or "..." run starting at i. A doubled quote is an
// escaped quote; with backslash set (E'...') a backslash escapes the next byte.
func skipQuoted(sql string, i int, quote byte, backslash bool) int {
	i++
	for i < len(sql) {
		switch {
		case backslash && sql[i] == '\\':
			i += 2
		case sql[i] == quote:
			if i+1 < len(sql) && sql[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		default:
			i++
		}
	}
	return len(sql)
}

// skipDollarQuoted skips $tag$ ... $tag$ when sql[i] opens one.
func skipDollarQuoted(sql string, i int) (int, bool) {
	j := i + 1
	for j < len(sql) && (isWordByte(sql[j]) && !(sql[j] >= '0' && sql[j] <= '9' && j == i+1)) {
		j++
	}
	if j >= len(sql) || sql[j] != '$' {
		return 0, false
	}
	tag := sql[i : j+1]
	if end := strings.Index(sql[j+1:], tag); end >= 0 {
		return j + 1 + end + len(tag), true
	}
	return len(sql), true
}
