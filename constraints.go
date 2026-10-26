package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
)

var constraintRegex = regexp.MustCompile("^  CONSTRAINT `((?:[^`]|``)+)` (?:(FOREIGN KEY|CHECK) )?")

// splitConstraints returns the foreign key definitions of a SHOW CREATE TABLE
// statement and the statement without them. Check constraints stay in the
// statement, renamed for the temp table: a generated name <table>_chk_<n>
// becomes <tempTable>_chk_<n>, which MySQL renames back to <table>_chk_<n>
// with the table, and any other name is dropped so MySQL generates one. The
// kind is the token after the name; the CHECK expression is not inspected.
func splitConstraints(createMySQL, tableName, tempTableName string) ([]foreignKey, string, error) {
	generated := regexp.MustCompile("^" + regexp.QuoteMeta(tableName) + `_chk_(\d+)$`)
	lines := strings.Split(createMySQL, "\n")
	kept := make([]string, 0, len(lines))
	var fks []foreignKey
	for _, line := range lines {
		m := constraintRegex.FindStringSubmatch(line)
		if m == nil {
			kept = append(kept, line)
			continue
		}
		name := strings.ReplaceAll(m[1], "``", "`")
		// the kind stays in the definition; only the name prefix is cut
		definition := strings.TrimPrefix(line, "  CONSTRAINT `"+m[1]+"` ")
		switch m[2] {
		case "FOREIGN KEY":
			fks = append(fks, foreignKey{Name: name, Definition: strings.TrimSuffix(definition, ",")})
			continue
		case "CHECK":
		default:
			return nil, "", fmt.Errorf("constraint %q is not a FOREIGN KEY or CHECK", name)
		}
		if n := generated.FindStringSubmatch(name); n != nil {
			kept = append(kept, "  CONSTRAINT "+quoteIdent(tempTableName+"_chk_"+n[1])+" "+definition)
			continue
		}
		log.Printf("check constraint %q is not named %s_chk_<n>; after the swap it gets a generated name", name, tableName)
		kept = append(kept, "  "+definition)
	}

	// the definitions are comma separated, so the last one left before the
	// closing paren must not end in a comma
	for i := len(kept) - 1; i > 0; i-- {
		if strings.HasPrefix(kept[i], ")") {
			kept[i-1] = strings.TrimSuffix(kept[i-1], ",")
			break
		}
	}
	return fks, strings.Join(kept, "\n"), nil
}

// The user's ALTER names a check on the original table. The temp table's
// generated checks are <temp>_chk_<n>, so that operand has to name the temp one.
func rewriteCheckNames(alterSQL, tableName, tempTableName string) string {
	// The name ends at a non-identifier character. Go's regexp has no lookahead,
	// so that character is kept and written back. t_chk_12a does not match.
	re := regexp.MustCompile(`(?i)(\b(?:drop\s+check|alter\s+check|drop\s+constraint|alter\s+constraint)\s+` + "`" + `?)` +
		regexp.QuoteMeta(tableName) + `(_chk_\d+` + "`" + `?(?:[^0-9A-Za-z_]|$))`)
	repl := "${1}" + strings.ReplaceAll(tempTableName, "$", "$$") + "${2}"
	var b strings.Builder
	for _, seg := range sqlPlainAndLocked(alterSQL) {
		if seg.locked {
			b.WriteString(seg.text)
			continue
		}
		b.WriteString(re.ReplaceAllString(seg.text, repl))
	}
	return b.String()
}

type sqlSegment struct {
	text   string
	locked bool
}

// sqlPlainAndLocked splits SQL into plain text and the string literals and
// comments a rewrite must not touch. A backtick-quoted identifier stays plain
// (the check rewrite has to see it) but is skipped whole, so a quote inside it
// doesn't open a string.
func sqlPlainAndLocked(s string) []sqlSegment {
	var segs []sqlSegment
	plainStart := 0
	for i := 0; i < len(s); {
		var end int
		switch {
		case s[i] == '`':
			i = scanQuoted(s, i, '`', false)
			continue
		case s[i] == '\'' || s[i] == '"':
			end = scanQuoted(s, i, s[i], true)
		case strings.HasPrefix(s[i:], "-- ") || s[i] == '#':
			end = scanLineComment(s, i)
		case strings.HasPrefix(s[i:], "/*"):
			end = len(s)
			if j := strings.Index(s[i+2:], "*/"); j >= 0 {
				end = i + 2 + j + 2
			}
		default:
			i++
			continue
		}
		if i > plainStart {
			segs = append(segs, sqlSegment{text: s[plainStart:i]})
		}
		segs = append(segs, sqlSegment{text: s[i:end], locked: true})
		i, plainStart = end, end
	}
	if len(s) > plainStart {
		segs = append(segs, sqlSegment{text: s[plainStart:]})
	}
	return segs
}

// scanQuoted returns the index just past the quote that closes the one at i.
// A doubled quote is part of the text; a backslash escapes only in strings.
func scanQuoted(s string, i int, q byte, backslash bool) int {
	for i++; i < len(s); i++ {
		if backslash && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == q {
			if i+1 < len(s) && s[i+1] == q {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(s)
}

func scanLineComment(s string, i int) int {
	if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(s)
}

// MySQL applies these actions by changing the rows itself and does not fire
// triggers, so the temp table cannot follow them.
func referentialActionSkipsTriggers(rule string) bool {
	switch strings.ToUpper(strings.TrimSpace(rule)) {
	case "CASCADE", "SET NULL", "SET DEFAULT":
		return true
	default:
		return false
	}
}

type referentialConstraint struct {
	Name       string
	UpdateRule string
	DeleteRule string
}

func queryReferentialConstraints(ctx context.Context, conn *sql.Conn, tableName string) ([]referentialConstraint, error) {
	rows, err := conn.QueryContext(ctx, "select `CONSTRAINT_NAME`,`UPDATE_RULE`,`DELETE_RULE`"+
		" from `information_schema`.`REFERENTIAL_CONSTRAINTS`"+
		" where `CONSTRAINT_SCHEMA`=database()"+
		" and `TABLE_NAME`=?", tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []referentialConstraint
	for rows.Next() {
		var fk referentialConstraint
		if err := rows.Scan(&fk.Name, &fk.UpdateRule, &fk.DeleteRule); err != nil {
			return nil, err
		}
		out = append(out, fk)
	}
	return out, rows.Err()
}
